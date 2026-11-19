package forum

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Input limits, in characters.
const (
	MaxTitleLen = 200
	MaxBodyLen  = 40000
)

// Page sizes for ListThreads.
const (
	DefaultPageSize = 25
	MaxPageSize     = 100
)

// Thread is a thread as one member sees it.
type Thread struct {
	ID     int64
	Title  string
	BodyMD string

	AuthorID     int64
	AuthorName   string
	AuthorAvatar string

	Score      int
	ReplyCount int
	Tags       []string

	CreatedAt      time.Time
	LastActivityAt time.Time
	EditedAt       *time.Time
	// DeletedAt is set when the author deleted the thread or an admin
	// removed it. Such a thread is absent from ListThreads.
	DeletedAt *time.Time

	// Voted reports whether the viewing member has upvoted the thread.
	Voted bool

	// FromSlack and the Slack fields are set for threads moved from Slack.
	FromSlack        bool
	SlackChannelName string
	SlackPermalink   string
	// SlackRemaining counts the replies that stayed in the Slack thread.
	SlackRemaining int
}

// Sort is a feed ordering.
type Sort int

const (
	// SortActivity orders by most recent activity. It is the default.
	SortActivity Sort = iota
	// SortScore orders by upvotes, newest first among equals.
	SortScore
)

// ListOptions selects a page of the feed.
type ListOptions struct {
	// Tag limits the feed to threads carrying this tag slug. Empty means all.
	Tag  string
	Sort Sort
	// Page is 1-based; anything below 1 means the first page.
	Page int
	// PageSize defaults to DefaultPageSize and is capped at MaxPageSize.
	PageSize int
	// ViewerID is the member whose votes fill Thread.Voted; 0 for none.
	ViewerID int64
}

// SlackSource describes the Slack thread a forum thread was moved from.
type SlackSource struct {
	ChannelID   string
	ChannelName string
	ThreadTS    string
	Permalink   string
	// Remaining counts the Slack replies that were not imported.
	Remaining int
	// MovedBy is the member who triggered the move.
	MovedBy int64
}

// newThread is the input to createThreadTx.
type newThread struct {
	AuthorID int64
	Title    string
	BodyMD   string
	TagSlugs []string
	// Slack is set only by the importer.
	Slack *SlackSource
}

// CreateThread stores a new thread by authorID and returns its ID. It
// returns a ValidationError when the title or body is empty or too long,
// no tag is given, or a tag is not on the active list. The thread is
// announced in Slack when a Notifier is set.
func (s *Store) CreateThread(ctx context.Context, authorID int64, title, bodyMD string, tagSlugs []string) (int64, error) {
	var id int64
	var enqueued bool
	err := s.db.WithWriteTx(ctx, func(tx *sql.Tx) error {
		var err error
		id, err = s.createThreadTx(ctx, tx, newThread{AuthorID: authorID, Title: title, BodyMD: bodyMD, TagSlugs: tagSlugs})
		if err != nil {
			return err
		}
		enqueued, err = s.enqueueAnnounceTx(ctx, tx, id)
		return err
	})
	if err != nil {
		return 0, err
	}
	s.wake(enqueued)
	return id, nil
}

// validThreadText trims a title and body and checks them against the
// limits.
func validThreadText(title, body string) (string, string, error) {
	title, body = strings.TrimSpace(title), strings.TrimSpace(body)
	switch {
	case title == "":
		return "", "", ValidationError{Field: "title", Message: "Give the thread a title."}
	case utf8.RuneCountInString(title) > MaxTitleLen:
		return "", "", ValidationError{Field: "title", Message: fmt.Sprintf("Keep the title to %d characters or fewer.", MaxTitleLen)}
	case body == "":
		return "", "", ValidationError{Field: "body", Message: "Write a body so people have something to answer."}
	case utf8.RuneCountInString(body) > MaxBodyLen:
		return "", "", ValidationError{Field: "body", Message: fmt.Sprintf("Keep the body to %d characters or fewer.", MaxBodyLen)}
	}
	return title, body, nil
}

// EditThread replaces a thread's title, body, and tags and marks it
// edited. Only the author may; anyone else gets ErrForbidden. A retired
// tag the thread already carries may stay on it.
func (s *Store) EditThread(ctx context.Context, memberID, id int64, title, bodyMD string, tagSlugs []string) error {
	return s.db.WithWriteTx(ctx, func(tx *sql.Tx) error {
		p, err := livePostTx(ctx, tx, PostRef{Kind: PostThread, ID: id})
		if err != nil {
			return err
		}
		if p.AuthorID != memberID {
			return ErrForbidden
		}
		title, body, err := validThreadText(title, bodyMD)
		if err != nil {
			return err
		}
		tagIDs, err := resolveTagsTx(ctx, tx, tagSlugs, id)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE threads SET title = ?, body_md = ?, edited_at = ? WHERE id = ?`,
			title, body, s.now().Unix(), id); err != nil {
			return fmt.Errorf("edit thread: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM thread_tags WHERE thread_id = ?`, id); err != nil {
			return fmt.Errorf("edit thread tags: %w", err)
		}
		return insertThreadTagsTx(ctx, tx, id, tagIDs)
	})
}

func insertThreadTagsTx(ctx context.Context, tx *sql.Tx, threadID int64, tagIDs []int64) error {
	for _, tagID := range tagIDs {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO thread_tags (thread_id, tag_id) VALUES (?, ?)`, threadID, tagID); err != nil {
			return fmt.Errorf("insert thread tag: %w", err)
		}
	}
	return nil
}

// createThreadTx validates and inserts a thread and its tags inside the
// caller's transaction. Anything that must commit together with the thread
// (a notification, imported replies) belongs in the same transaction.
func (s *Store) createThreadTx(ctx context.Context, tx *sql.Tx, in newThread) (int64, error) {
	title, body, err := validThreadText(in.Title, in.BodyMD)
	if err != nil {
		return 0, err
	}
	tagIDs, err := resolveTagsTx(ctx, tx, in.TagSlugs, 0)
	if err != nil {
		return 0, err
	}

	now := s.now().Unix()
	var channelID, channelName, threadTS, permalink, remaining, movedBy any
	if in.Slack != nil {
		channelID, channelName, threadTS = in.Slack.ChannelID, in.Slack.ChannelName, in.Slack.ThreadTS
		permalink, remaining, movedBy = in.Slack.Permalink, in.Slack.Remaining, in.Slack.MovedBy
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO threads (author_id, title, body_md, created_at, last_activity_at,
			slack_channel_id, slack_channel_name, slack_thread_ts, slack_permalink, slack_remaining, moved_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.AuthorID, title, body, now, now,
		channelID, channelName, threadTS, permalink, remaining, movedBy)
	if err != nil {
		return 0, fmt.Errorf("insert thread: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("insert thread: %w", err)
	}
	if err := insertThreadTagsTx(ctx, tx, id, tagIDs); err != nil {
		return 0, err
	}
	return id, nil
}

// threadColumns is the select list scanThread reads. Its one placeholder
// is the viewing member's ID.
const threadColumns = `
	t.id, t.title, t.body_md, t.author_id, m.name, m.avatar_url,
	t.score, t.reply_count, t.created_at, t.last_activity_at, t.edited_at, t.deleted_at,
	EXISTS (SELECT 1 FROM votes v
	        WHERE v.member_id = ? AND v.target_kind = 'thread' AND v.target_id = t.id),
	t.slack_channel_id IS NOT NULL,
	COALESCE(t.slack_channel_name, ''), COALESCE(t.slack_permalink, ''), COALESCE(t.slack_remaining, 0)
	FROM threads t JOIN members m ON m.id = t.author_id`

func scanThread(row interface{ Scan(...any) error }) (Thread, error) {
	var t Thread
	var created, activity int64
	var edited, deleted *int64
	err := row.Scan(&t.ID, &t.Title, &t.BodyMD, &t.AuthorID, &t.AuthorName, &t.AuthorAvatar,
		&t.Score, &t.ReplyCount, &created, &activity, &edited, &deleted,
		&t.Voted, &t.FromSlack, &t.SlackChannelName, &t.SlackPermalink, &t.SlackRemaining)
	if err != nil {
		return Thread{}, err
	}
	t.CreatedAt, t.LastActivityAt = time.Unix(created, 0), time.Unix(activity, 0)
	t.EditedAt, t.DeletedAt = unixPtr(edited), unixPtr(deleted)
	return t, nil
}

// GetThread returns one thread as viewerID sees it, or ErrNotFound. A
// deleted thread is returned with DeletedAt set so its page can say so.
func (s *Store) GetThread(ctx context.Context, id, viewerID int64) (Thread, error) {
	t, err := scanThread(s.db.Reader().QueryRowContext(ctx,
		`SELECT `+threadColumns+` WHERE t.id = ?`, viewerID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Thread{}, ErrNotFound
	}
	if err != nil {
		return Thread{}, fmt.Errorf("get thread: %w", err)
	}
	one := []Thread{t}
	if err := s.loadTags(ctx, one); err != nil {
		return Thread{}, err
	}
	return one[0], nil
}

// ListThreads returns one page of live threads and whether more follow. A
// page past the end is empty, not an error.
func (s *Store) ListThreads(ctx context.Context, opts ListOptions) ([]Thread, bool, error) {
	page, size := max(opts.Page, 1), opts.PageSize
	if size < 1 {
		size = DefaultPageSize
	}
	size = min(size, MaxPageSize)

	query := `SELECT ` + threadColumns + ` WHERE t.deleted_at IS NULL`
	args := []any{opts.ViewerID}
	if opts.Tag != "" {
		query += ` AND EXISTS (SELECT 1 FROM thread_tags tt JOIN tags g ON g.id = tt.tag_id
		                       WHERE tt.thread_id = t.id AND g.slug = ?)`
		args = append(args, opts.Tag)
	}
	if opts.Sort == SortScore {
		query += ` ORDER BY t.score DESC, t.created_at DESC, t.id DESC`
	} else {
		query += ` ORDER BY t.last_activity_at DESC, t.id DESC`
	}
	// One extra row tells us whether another page exists.
	query += ` LIMIT ? OFFSET ?`
	args = append(args, size+1, (page-1)*size)

	rows, err := s.db.Reader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, fmt.Errorf("list threads: %w", err)
	}
	defer rows.Close()
	var threads []Thread
	for rows.Next() {
		t, err := scanThread(rows)
		if err != nil {
			return nil, false, fmt.Errorf("list threads: %w", err)
		}
		threads = append(threads, t)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("list threads: %w", err)
	}
	rows.Close()

	hasMore := len(threads) > size
	if hasMore {
		threads = threads[:size]
	}
	if err := s.loadTags(ctx, threads); err != nil {
		return nil, false, err
	}
	return threads, hasMore, nil
}
