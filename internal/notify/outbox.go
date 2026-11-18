// Package notify is the outbox for the forum's Slack messages (plan KTD6).
//
// A post enqueues its message in the same transaction that stores the
// post, and Worker delivers it afterwards with bounded retries, so a
// Slack outage never fails a post or silently loses a notification.
package notify

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Kind says why a message is being sent.
type Kind string

// The kinds match the notifications.kind CHECK constraint.
const (
	KindAnnounce   Kind = "announce"
	KindReplyDM    Kind = "reply_dm"
	KindLinkBack   Kind = "link_back"
	KindMoveFailed Kind = "move_failed"
)

// Message is what gets delivered; it is stored as the row's JSON payload.
// Channel is a channel ID, or a user ID for a direct message.
type Message struct {
	Channel  string `json:"channel"`
	ThreadTS string `json:"thread_ts,omitempty"`
	Text     string `json:"text"`
}

// Enqueue writes one outbox row inside the caller's transaction. The row
// is due immediately.
func Enqueue(ctx context.Context, tx *sql.Tx, now time.Time, kind Kind, msg Message) error {
	switch kind {
	case KindAnnounce, KindReplyDM, KindLinkBack, KindMoveFailed:
	default:
		return fmt.Errorf("notify: unknown kind %q", kind)
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("notify: encode payload: %w", err)
	}
	ts := now.Unix()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO notifications (kind, payload, next_attempt_at, created_at) VALUES (?, ?, ?, ?)`,
		string(kind), string(payload), ts, ts,
	); err != nil {
		return fmt.Errorf("notify: enqueue %s: %w", kind, err)
	}
	return nil
}

// excerptMax is the longest reply excerpt quoted in a direct message, in
// characters.
const excerptMax = 280

// AnnounceText is the channel announcement for a new forum thread.
func AnnounceText(authorSlackID, title, threadURL string, tags []string) string {
	lines := []string{
		mention(authorSlackID) + " started a thread.",
		link(threadURL, title),
	}
	var hashed []string
	for _, tag := range tags {
		if tag = oneLine(tag); tag != "" {
			hashed = append(hashed, "#"+escape(tag))
		}
	}
	if len(hashed) > 0 {
		lines = append(lines, strings.Join(hashed, " "))
	}
	return strings.Join(lines, "\n")
}

// ReplyDMText is the direct message sent to a member whose post got a
// reply. The excerpt is flattened to one line and capped.
func ReplyDMText(replierName, threadTitle, excerpt, replyURL string) string {
	lines := []string{escape(oneLine(replierName)) + " replied to you in *" + escape(oneLine(threadTitle)) + "*"}
	if excerpt = truncate(oneLine(excerpt), excerptMax); excerpt != "" {
		lines = append(lines, "> "+escape(excerpt))
	}
	lines = append(lines,
		"<"+replyURL+"|Read and reply →>",
		"Turn these off in your forum settings.",
	)
	return strings.Join(lines, "\n")
}

// LinkBackText is posted into a Slack thread after it moves to the forum.
// imported and remaining count replies, not the original post.
func LinkBackText(moverSlackID string, imported, remaining int, title, threadURL string) string {
	var went, stayed string
	switch imported {
	case 0:
		went = "No replies went with it."
	case 1:
		went = "1 reply that answers the original post went with it."
	default:
		went = strconv.Itoa(imported) + " replies that answer the original post went with it."
	}
	switch remaining {
	case 0:
		stayed = "No replies stayed here."
	case 1:
		stayed = "1 reply stayed here."
	default:
		stayed = strconv.Itoa(remaining) + " replies stayed here."
	}
	return strings.Join([]string{
		mention(moverSlackID) + " moved this thread to the forum.",
		went + " " + stayed,
		link(threadURL, title),
		"Carry on there: replies nest and the best ones rise.",
	}, "\n")
}

// MoveFailedText tells the member who asked for a move that it did not
// happen.
func MoveFailedText(title string) string {
	subject := "this thread"
	if title = oneLine(title); title != "" {
		subject = "*" + escape(title) + "*"
	}
	return "Moving " + subject + " to the forum did not work. Nothing was imported and the thread here is unchanged. You can try again."
}

// mention renders a Slack user mention.
func mention(slackID string) string { return "<@" + slackID + ">" }

// link renders a titled link with the design's trailing arrow.
func link(url, title string) string {
	return "<" + url + "|" + escape(oneLine(title)) + " →>"
}

// escape neutralises Slack's three control characters so member text
// cannot form markup, links, or mentions such as <!channel>.
func escape(s string) string {
	return slackEscaper.Replace(s)
}

var slackEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// oneLine collapses every run of whitespace, including newlines, to a
// single space.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// truncate cuts s to at most max characters, ending with an ellipsis
// when something was cut.
func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return strings.TrimRight(string(r[:max-1]), " ") + "…"
}
