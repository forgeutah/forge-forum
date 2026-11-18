package forum

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"

	"github.com/forgeutah/forge-forum/internal/notify"
)

// Notifier configures the Slack messages posts cause (plan KTD6). The
// zero value turns them off: nothing is enqueued.
type Notifier struct {
	// BaseURL is the forum's public address with no trailing slash, used
	// to build links. Notifications are on when it is set.
	BaseURL string
	// AnnounceChannel is the Slack channel new threads are announced in.
	// Announcements are enqueued even when it is empty; the worker marks
	// those rows skipped.
	AnnounceChannel string
	// Wake is called after a post that enqueued something has committed,
	// so the worker delivers at once. It may be nil.
	Wake func()
}

// SetNotifier turns notifications on or off. Call it before the store is
// shared between goroutines.
func (s *Store) SetNotifier(n Notifier) { s.notifier = n }

// enabled reports whether posts should enqueue Slack messages.
func (n Notifier) enabled() bool { return n.BaseURL != "" }

// wake nudges the worker after a transaction that may have enqueued
// something has committed.
func (s *Store) wake(enqueued bool) {
	if enqueued && s.notifier.Wake != nil {
		s.notifier.Wake()
	}
}

func threadPath(id int64) string { return "/threads/" + strconv.FormatInt(id, 10) }

func replyPath(threadID, replyID int64) string {
	return threadPath(threadID) + "#reply-" + strconv.FormatInt(replyID, 10)
}

// enqueueAnnounceTx queues the channel announcement for a thread created
// on the forum (plan R11) and reports whether it queued one. The importer
// does not call it: a moved thread gets a link-back instead (R18).
func (s *Store) enqueueAnnounceTx(ctx context.Context, tx *sql.Tx, threadID int64) (bool, error) {
	if !s.notifier.enabled() {
		return false, nil
	}
	var title, authorSlackID string
	if err := tx.QueryRowContext(ctx, `
		SELECT t.title, m.slack_user_id FROM threads t JOIN members m ON m.id = t.author_id
		WHERE t.id = ?`, threadID).Scan(&title, &authorSlackID); err != nil {
		return false, fmt.Errorf("announce thread: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT g.slug FROM thread_tags tt JOIN tags g ON g.id = tt.tag_id
		WHERE tt.thread_id = ? ORDER BY g.id`, threadID)
	if err != nil {
		return false, fmt.Errorf("announce thread: %w", err)
	}
	defer rows.Close()
	var tags []string
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return false, fmt.Errorf("announce thread: %w", err)
		}
		tags = append(tags, slug)
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("announce thread: %w", err)
	}
	rows.Close()

	err = notify.Enqueue(ctx, tx, s.now(), notify.KindAnnounce, notify.Message{
		Channel: s.notifier.AnnounceChannel,
		Text:    notify.AnnounceText(authorSlackID, title, s.notifier.BaseURL+threadPath(threadID), tags),
	})
	return err == nil, err
}

// enqueueReplyDMTx queues a direct message to the author of the post a
// new reply answers (plan R12) and reports whether it queued one. Nobody
// is messaged about their own reply, and members who turned the messages
// off (R13) are left alone.
func (s *Store) enqueueReplyDMTx(ctx context.Context, tx *sql.Tx, replyID int64, in newReply, answeredAuthorID int64) (bool, error) {
	if !s.notifier.enabled() || answeredAuthorID == in.AuthorID {
		return false, nil
	}
	var slackID string
	var wantsDMs bool
	if err := tx.QueryRowContext(ctx,
		`SELECT slack_user_id, dm_replies FROM members WHERE id = ?`, answeredAuthorID).Scan(&slackID, &wantsDMs); err != nil {
		return false, fmt.Errorf("notify reply: %w", err)
	}
	if !wantsDMs {
		return false, nil
	}
	var replier, title string
	if err := tx.QueryRowContext(ctx, `
		SELECT (SELECT name FROM members WHERE id = ?), (SELECT title FROM threads WHERE id = ?)`,
		in.AuthorID, in.ThreadID).Scan(&replier, &title); err != nil {
		return false, fmt.Errorf("notify reply: %w", err)
	}
	err := notify.Enqueue(ctx, tx, s.now(), notify.KindReplyDM, notify.Message{
		Channel: slackID,
		Text:    notify.ReplyDMText(replier, title, in.BodyMD, s.notifier.BaseURL+replyPath(in.ThreadID, replyID)),
	})
	return err == nil, err
}
