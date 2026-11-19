package forum

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Actor is the member performing an action. IsAdmin comes from the
// request (forge-proxy's roles header) and is never stored.
type Actor struct {
	ID      int64
	IsAdmin bool
}

// PostKind says whether a post is a thread or a reply. The values match
// the votes.target_kind CHECK constraint.
type PostKind string

const (
	PostThread PostKind = "thread"
	PostReply  PostKind = "reply"
)

// PostRef names one thread or reply.
type PostRef struct {
	Kind PostKind
	ID   int64
}

// table is the post's table. It is the only place a table name is chosen
// at run time, and always from these constants.
func (k PostKind) table() (string, error) {
	switch k {
	case PostThread:
		return "threads", nil
	case PostReply:
		return "replies", nil
	}
	return "", fmt.Errorf("forum: unknown post kind %q", k)
}

// postRow is what every action on a post needs to know about it.
type postRow struct {
	ThreadID int64
	AuthorID int64
}

// livePostTx loads a post that can still be acted on. It returns
// ErrNotFound when the post does not exist and ErrDeleted when it, or the
// thread it belongs to, was deleted.
func livePostTx(ctx context.Context, tx *sql.Tx, post PostRef) (postRow, error) {
	var p postRow
	var deleted, threadDeleted *int64
	var err error
	switch post.Kind {
	case PostThread:
		p.ThreadID = post.ID
		err = tx.QueryRowContext(ctx,
			`SELECT author_id, deleted_at FROM threads WHERE id = ?`, post.ID).Scan(&p.AuthorID, &deleted)
	case PostReply:
		err = tx.QueryRowContext(ctx, `
			SELECT r.thread_id, r.author_id, r.deleted_at, t.deleted_at
			FROM replies r JOIN threads t ON t.id = r.thread_id
			WHERE r.id = ?`, post.ID).Scan(&p.ThreadID, &p.AuthorID, &deleted, &threadDeleted)
	default:
		_, err = post.Kind.table()
		return postRow{}, err
	}
	if errors.Is(err, sql.ErrNoRows) {
		return postRow{}, ErrNotFound
	}
	if err != nil {
		return postRow{}, fmt.Errorf("load %s: %w", post.Kind, err)
	}
	if deleted != nil || threadDeleted != nil {
		return postRow{}, ErrDeleted
	}
	return p, nil
}

// Reply is one reply's own fields, for the edit form.
type Reply struct {
	ID       int64
	ThreadID int64
	AuthorID int64
	BodyMD   string
}

// newReply is the input to createReplyTx.
type newReply struct {
	AuthorID int64
	ThreadID int64
	// ParentID is the reply being answered, or 0 for the thread itself.
	ParentID int64
	BodyMD   string
}

// validReplyBody trims a reply body and checks it against the limit.
func validReplyBody(body string) (string, error) {
	body = strings.TrimSpace(body)
	switch {
	case body == "":
		return "", ValidationError{Field: "body", Message: "Write something before you post."}
	case utf8.RuneCountInString(body) > MaxBodyLen:
		return "", ValidationError{Field: "body", Message: fmt.Sprintf("Keep the reply to %d characters or fewer.", MaxBodyLen)}
	}
	return body, nil
}

// CreateReply stores authorID's reply to a thread (parentID 0) or to one
// of its replies and returns the new reply's ID. It returns ErrNotFound
// when the thread or parent does not exist or the parent belongs to
// another thread, ErrDeleted when either was deleted, and a
// ValidationError for an empty or oversized body. The author of the post
// being answered gets a Slack message when a Notifier is set, and the
// reply listener, if any, hears about the reply once it has committed.
func (s *Store) CreateReply(ctx context.Context, authorID, threadID, parentID int64, bodyMD string) (int64, error) {
	in := newReply{AuthorID: authorID, ThreadID: threadID, ParentID: parentID, BodyMD: bodyMD}
	var id int64
	var enqueued bool
	err := s.db.WithWriteTx(ctx, func(tx *sql.Tx) error {
		var answered postRow
		var err error
		id, answered, err = s.createReplyTx(ctx, tx, in)
		if err != nil {
			return err
		}
		enqueued, err = s.enqueueReplyDMTx(ctx, tx, id, in, answered.AuthorID)
		return err
	})
	if err != nil {
		return 0, err
	}
	s.wake(enqueued)
	if s.replyListener != nil {
		s.replyListener(threadID, id, parentID, authorID)
	}
	return id, nil
}

// createReplyTx validates and inserts a reply and updates the thread's
// reply count and activity time, all inside the caller's transaction. It
// also returns the post that was answered.
func (s *Store) createReplyTx(ctx context.Context, tx *sql.Tx, in newReply) (int64, postRow, error) {
	target := PostRef{Kind: PostThread, ID: in.ThreadID}
	var parent any
	if in.ParentID != 0 {
		target, parent = PostRef{Kind: PostReply, ID: in.ParentID}, in.ParentID
	}
	answered, err := livePostTx(ctx, tx, target)
	if err != nil {
		return 0, postRow{}, err
	}
	if answered.ThreadID != in.ThreadID {
		return 0, postRow{}, ErrNotFound
	}
	body, err := validReplyBody(in.BodyMD)
	if err != nil {
		return 0, postRow{}, err
	}

	now := s.now().Unix()
	res, err := tx.ExecContext(ctx,
		`INSERT INTO replies (thread_id, parent_id, author_id, body_md, created_at) VALUES (?, ?, ?, ?, ?)`,
		in.ThreadID, parent, in.AuthorID, body, now)
	if err != nil {
		return 0, postRow{}, fmt.Errorf("insert reply: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, postRow{}, fmt.Errorf("insert reply: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE threads SET reply_count = reply_count + 1, last_activity_at = ? WHERE id = ?`,
		now, in.ThreadID); err != nil {
		return 0, postRow{}, fmt.Errorf("bump thread: %w", err)
	}
	return id, answered, nil
}

// GetReply returns one live reply, ErrNotFound, or ErrDeleted.
func (s *Store) GetReply(ctx context.Context, id int64) (Reply, error) {
	r := Reply{ID: id}
	var deleted, threadDeleted *int64
	err := s.db.Reader().QueryRowContext(ctx, `
		SELECT r.thread_id, r.author_id, r.body_md, r.deleted_at, t.deleted_at
		FROM replies r JOIN threads t ON t.id = r.thread_id
		WHERE r.id = ?`, id).Scan(&r.ThreadID, &r.AuthorID, &r.BodyMD, &deleted, &threadDeleted)
	if errors.Is(err, sql.ErrNoRows) {
		return Reply{}, ErrNotFound
	}
	if err != nil {
		return Reply{}, fmt.Errorf("get reply: %w", err)
	}
	if deleted != nil || threadDeleted != nil {
		return Reply{}, ErrDeleted
	}
	return r, nil
}

// EditReply replaces a reply's body, marks it edited, and returns its
// thread's ID. Only the author may; anyone else gets ErrForbidden.
func (s *Store) EditReply(ctx context.Context, memberID, id int64, bodyMD string) (int64, error) {
	var threadID int64
	err := s.db.WithWriteTx(ctx, func(tx *sql.Tx) error {
		p, err := livePostTx(ctx, tx, PostRef{Kind: PostReply, ID: id})
		if err != nil {
			return err
		}
		if p.AuthorID != memberID {
			return ErrForbidden
		}
		body, err := validReplyBody(bodyMD)
		if err != nil {
			return err
		}
		threadID = p.ThreadID
		if _, err := tx.ExecContext(ctx,
			`UPDATE replies SET body_md = ?, edited_at = ? WHERE id = ?`, body, s.now().Unix(), id); err != nil {
			return fmt.Errorf("edit reply: %w", err)
		}
		return nil
	})
	return threadID, err
}

// DeletePost soft-deletes a thread or reply and returns its thread's ID.
// The author deletes their own post; an admin removes anyone's, which is
// recorded as a removal. Anyone else gets ErrForbidden. Deleting a reply
// keeps the thread's reply count equal to its live replies.
func (s *Store) DeletePost(ctx context.Context, actor Actor, post PostRef) (int64, error) {
	var threadID int64
	err := s.db.WithWriteTx(ctx, func(tx *sql.Tx) error {
		table, err := post.Kind.table()
		if err != nil {
			return err
		}
		p, err := livePostTx(ctx, tx, post)
		if err != nil {
			return err
		}
		var removedBy any
		switch {
		case p.AuthorID == actor.ID:
		case actor.IsAdmin:
			removedBy = actor.ID
		default:
			return ErrForbidden
		}
		threadID = p.ThreadID
		if _, err := tx.ExecContext(ctx,
			`UPDATE `+table+` SET deleted_at = ?, removed_by = ? WHERE id = ?`,
			s.now().Unix(), removedBy, post.ID); err != nil {
			return fmt.Errorf("delete %s: %w", post.Kind, err)
		}
		if post.Kind == PostReply {
			if _, err := tx.ExecContext(ctx,
				`UPDATE threads SET reply_count = reply_count - 1 WHERE id = ?`, threadID); err != nil {
				return fmt.Errorf("delete reply: %w", err)
			}
		}
		return nil
	})
	return threadID, err
}
