package forum

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/forgeutah/forge-forum/internal/identity"
	"github.com/forgeutah/forge-forum/internal/notify"
)

// ImportedReply is one Slack reply to import as a first-level forum reply.
type ImportedReply struct {
	SlackUserID  string
	AuthorName   string
	AuthorAvatar string
	BodyMD       string
	SlackTS      string
	CreatedAt    time.Time
}

// ImportInput describes a Slack thread being moved to the forum.
type ImportInput struct {
	Title    string
	TagSlugs []string

	// The member who triggered the move. They may never have opened the
	// forum, so they are identified by Slack user ID and get a member row
	// if they have none.
	MoverSlackID string
	MoverName    string
	MoverAvatar  string

	ChannelID   string
	ChannelName string
	ThreadTS    string
	Permalink   string

	// The original post, which becomes the thread body under its author.
	AuthorSlackID string
	AuthorName    string
	AuthorAvatar  string
	BodyMD        string
	PostedAt      time.Time

	// Replies holds the direct replies only, in their original order.
	Replies []ImportedReply
	// Remaining counts the Slack replies that are not imported.
	Remaining int
	// ThreadURL returns the absolute forum URL of a thread, for the
	// link-back posted into the Slack thread.
	ThreadURL func(threadID int64) string
}

// ImportSlackThread creates the forum thread and its imported replies in
// one transaction, and enqueues the link-back into the Slack thread in
// that same transaction. A moved thread gets no channel announcement and
// its replies send no direct messages.
//
// If the Slack thread was already moved it returns the existing thread's
// ID with existing set and writes nothing, including when two moves race.
// A title or tag problem comes back as a ValidationError.
func (s *Store) ImportSlackThread(ctx context.Context, in ImportInput) (threadID int64, existing bool, err error) {
	if in.ChannelID == "" || in.ThreadTS == "" {
		return 0, false, errors.New("import: missing Slack channel or thread timestamp")
	}
	if in.ThreadURL == nil {
		return 0, false, errors.New("import: missing ThreadURL")
	}
	err = s.db.WithWriteTx(ctx, func(tx *sql.Tx) error {
		var err error
		threadID, existing, err = importLookup(ctx, tx, in.ChannelID, in.ThreadTS)
		if err != nil || existing {
			return err
		}
		threadID, err = s.importInsertTx(ctx, tx, in)
		return err
	})
	if err != nil {
		return s.importResolveConflict(ctx, in, err)
	}
	return threadID, existing, nil
}

// SlackThreadID returns the forum thread already created from this Slack
// thread, if any. A removed thread still counts: it was moved once.
func (s *Store) SlackThreadID(ctx context.Context, channelID, threadTS string) (int64, bool, error) {
	return importLookup(ctx, s.db.Reader(), channelID, threadTS)
}

// importLookup finds the thread moved from a Slack thread. q is the reader
// pool or an open transaction.
func importLookup(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, channelID, threadTS string) (int64, bool, error) {
	var id int64
	err := q.QueryRowContext(ctx,
		`SELECT id FROM threads WHERE slack_channel_id = ? AND slack_thread_ts = ?`,
		channelID, threadTS).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("look up moved thread: %w", err)
	}
	return id, true, nil
}

// importResolveConflict decides what a failed import means. When the Slack
// thread has a forum thread after all, another move won the race (the
// unique index on the Slack source fired) and that thread is the answer.
func (s *Store) importResolveConflict(ctx context.Context, in ImportInput, cause error) (int64, bool, error) {
	var ve ValidationError
	if errors.As(cause, &ve) {
		return 0, false, cause
	}
	// The caller's context may be the reason the import failed.
	id, found, err := s.SlackThreadID(context.WithoutCancel(ctx), in.ChannelID, in.ThreadTS)
	if err == nil && found {
		return id, true, nil
	}
	return 0, false, cause
}

// importInsertTx writes the thread, its imported replies, and the
// link-back notification.
func (s *Store) importInsertTx(ctx context.Context, tx *sql.Tx, in ImportInput) (int64, error) {
	// Each Slack user is resolved once per import. The first details seen
	// for a user are the ones a new member row gets.
	now := s.now()
	members := map[string]int64{}
	ensure := func(slackUserID, name, avatar string) (int64, error) {
		if id, ok := members[slackUserID]; ok {
			return id, nil
		}
		id, err := identity.EnsureBySlackID(ctx, tx, slackUserID, name, avatar, now)
		if err != nil {
			return 0, err
		}
		members[slackUserID] = id
		return id, nil
	}
	authorID, err := ensure(in.AuthorSlackID, in.AuthorName, in.AuthorAvatar)
	if err != nil {
		return 0, err
	}
	moverID, err := ensure(in.MoverSlackID, in.MoverName, in.MoverAvatar)
	if err != nil {
		return 0, err
	}

	title := strings.TrimSpace(in.Title)
	threadID, err := s.createThreadTx(ctx, tx, newThread{
		AuthorID: authorID,
		Title:    title,
		BodyMD:   in.BodyMD,
		TagSlugs: in.TagSlugs,
		Slack: &SlackSource{
			ChannelID:   in.ChannelID,
			ChannelName: in.ChannelName,
			ThreadTS:    in.ThreadTS,
			Permalink:   in.Permalink,
			Remaining:   in.Remaining,
			MovedBy:     moverID,
		},
	})
	if err != nil {
		return 0, err
	}
	// createThreadTx stamps the thread with the time of the move. Keep that
	// as the last activity, so the thread surfaces in the feed, but date
	// the post itself to when it was written in Slack.
	if _, err := tx.ExecContext(ctx,
		`UPDATE threads SET created_at = ?, reply_count = ? WHERE id = ?`,
		in.PostedAt.Unix(), len(in.Replies), threadID); err != nil {
		return 0, fmt.Errorf("import thread: %w", err)
	}

	for _, r := range in.Replies {
		replyAuthor, err := ensure(r.SlackUserID, r.AuthorName, r.AuthorAvatar)
		if err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO replies (thread_id, author_id, body_md, created_at, imported, slack_ts)
			VALUES (?, ?, ?, ?, 1, ?)`,
			threadID, replyAuthor, strings.TrimSpace(r.BodyMD), r.CreatedAt.Unix(), r.SlackTS); err != nil {
			return 0, fmt.Errorf("import reply: %w", err)
		}
	}

	err = notify.Enqueue(ctx, tx, s.now(), notify.KindLinkBack, notify.Message{
		Channel:  in.ChannelID,
		ThreadTS: in.ThreadTS,
		Text:     notify.LinkBackText(in.MoverSlackID, len(in.Replies), in.Remaining, title, in.ThreadURL(threadID)),
	})
	if err != nil {
		return 0, err
	}
	return threadID, nil
}
