package move

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/forgeutah/forge-forum/internal/forum"
	"github.com/forgeutah/forge-forum/internal/markdown"
	"github.com/forgeutah/forge-forum/internal/notify"
	"github.com/forgeutah/forge-forum/internal/slackx"
)

// job is one confirmed move: the Slack thread and what the member chose
// in the dialog.
type job struct {
	channelID string
	rootTS    string
	title     string
	tags      []string
	moverID   string // Slack user ID
	moverName string // Slack handle, used when the profile cannot be read
}

// run moves one Slack thread. Either the whole thread is imported or
// nothing is: on any failure the mover gets a direct message saying so.
func (m *Mover) run(ctx context.Context, j job) {
	err := m.move(ctx, j)
	if err == nil {
		return
	}
	// Errors here carry Slack error codes and stage names, never message text.
	m.d.Logger.Error("move to forum failed", "channel", j.channelID, "thread_ts", j.rootTS, "err", err)

	// The job's context may be what failed; the mover is told regardless.
	ctx = context.WithoutCancel(ctx)
	err = m.d.DB.WithWriteTx(ctx, func(tx *sql.Tx) error {
		return notify.Enqueue(ctx, tx, m.d.Now(), notify.KindMoveFailed, notify.Message{
			Channel: j.moverID,
			Text:    notify.MoveFailedText(j.title),
		})
	})
	if err != nil {
		m.d.Logger.Error("move failure could not be reported to the mover", "err", err)
		return
	}
	m.wake()
}

// move reads the Slack thread, has its replies classified, and imports
// the original post with the replies that answer it directly. A refusal
// (the thread was already moved, the channel is not public) is told to
// the mover and is not an error.
func (m *Mover) move(ctx context.Context, j job) error {
	// The dialog can sit open; check again what the shortcut checked.
	channel, refusal, err := m.refusal(ctx, j.channelID, j.rootTS)
	if err != nil {
		return err
	}
	if refusal != "" {
		return m.tell(ctx, j.moverID, refusal)
	}

	msgs, err := m.d.Slack.ThreadReplies(ctx, j.channelID, j.rootTS)
	if err != nil {
		return fmt.Errorf("read thread: %w", err)
	}
	if len(msgs) == 0 {
		return errors.New("read thread: Slack returned no messages")
	}
	root, replies := msgs[0], msgs[1:]
	if root.TS != j.rootTS {
		// The dialog was opened from a reply that did not name its root.
		// Slack has now told us the real one, which may already be moved.
		id, moved, err := m.d.Store.SlackThreadID(ctx, j.channelID, root.TS)
		if err != nil {
			return err
		}
		if moved {
			return m.tell(ctx, j.moverID, m.alreadyMovedText(id))
		}
	}

	if !memberMessage(root) {
		return m.tell(ctx, j.moverID, notMemberText)
	}
	users := newUserCache(ctx, m.d.Slack)
	author, _ := users.get(root.User)
	if author.IsBot {
		return m.tell(ctx, j.moverID, notMemberText)
	}

	// Replies a member did not write (apps, bots, "joined the channel")
	// and replies with no text are never imported. They are not shown to
	// the classifier and count among the replies left in Slack.
	type candidate struct {
		msg    slackx.Message
		author slackx.User
		bodyMD string
	}
	var candidates []candidate
	var toClassify []SlackMessage
	for _, r := range replies {
		if !memberMessage(r) {
			continue
		}
		author, _ := users.get(r.User)
		body := strings.TrimSpace(markdown.FromSlack(r.Text, users))
		if author.IsBot || body == "" {
			continue
		}
		candidates = append(candidates, candidate{msg: r, author: author, bodyMD: body})
		toClassify = append(toClassify, SlackMessage{ID: r.TS, Author: author.Name, Text: body})
	}

	bodyMD := markdown.FromSlack(root.Text, users)
	var direct map[string]bool
	if len(toClassify) > 0 {
		direct, err = m.d.Classifier.Classify(ctx, SlackMessage{ID: root.TS, Author: author.Name, Text: bodyMD}, toClassify)
		if err != nil {
			return fmt.Errorf("classify replies: %w", err)
		}
	}
	var imported []forum.ImportedReply
	for _, c := range candidates {
		if !direct[c.msg.TS] {
			continue
		}
		imported = append(imported, forum.ImportedReply{
			SlackUserID:  c.author.ID,
			AuthorName:   c.author.Name,
			AuthorAvatar: c.author.AvatarURL,
			BodyMD:       c.bodyMD,
			SlackTS:      c.msg.TS,
			CreatedAt:    m.slackTime(c.msg.TS),
		})
	}

	permalink, err := m.d.Slack.Permalink(ctx, j.channelID, root.TS)
	if err != nil {
		return fmt.Errorf("read permalink: %w", err)
	}
	mover, found := users.get(j.moverID)
	if !found && j.moverName != "" {
		mover.Name = j.moverName
	}

	threadID, existing, err := m.d.Store.ImportSlackThread(ctx, forum.ImportInput{
		Title:         j.title,
		TagSlugs:      j.tags,
		MoverSlackID:  j.moverID,
		MoverName:     mover.Name,
		MoverAvatar:   mover.AvatarURL,
		ChannelID:     j.channelID,
		ChannelName:   channel.Name,
		ThreadTS:      root.TS,
		Permalink:     permalink,
		AuthorSlackID: author.ID,
		AuthorName:    author.Name,
		AuthorAvatar:  author.AvatarURL,
		BodyMD:        bodyMD,
		PostedAt:      m.slackTime(root.TS),
		Replies:       imported,
		Remaining:     len(replies) - len(imported),
		ThreadURL:     m.threadURL,
	})
	if err != nil {
		return fmt.Errorf("import thread: %w", err)
	}
	if existing {
		// Another move of this thread finished first.
		return m.tell(ctx, j.moverID, m.alreadyMovedText(threadID))
	}
	// The link-back into the Slack thread is already in the outbox.
	m.wake()
	return nil
}

func (m *Mover) wake() {
	if m.d.Wake != nil {
		m.d.Wake()
	}
}

// slackTime converts a Slack message timestamp ("1760950800.000100") to
// the time it was posted, or the current time when it cannot be read.
func (m *Mover) slackTime(ts string) time.Time {
	seconds, _, _ := strings.Cut(ts, ".")
	unix, err := strconv.ParseInt(seconds, 10, 64)
	if err != nil || unix <= 0 {
		return m.d.Now()
	}
	return time.Unix(unix, 0)
}

// userCache looks Slack users up once per job. It is the resolver for
// mentions in message text as well as the source of author profiles. It
// is used by one goroutine and holds that job's context.
type userCache struct {
	ctx   context.Context
	slack slackx.Client
	users map[string]slackx.User
	found map[string]bool
}

func newUserCache(ctx context.Context, client slackx.Client) *userCache {
	return &userCache{ctx: ctx, slack: client, users: map[string]slackx.User{}, found: map[string]bool{}}
}

// get returns a user's profile. When Slack cannot supply it, the user is
// still credited, under their Slack user ID, and found is false.
func (c *userCache) get(userID string) (user slackx.User, found bool) {
	if u, ok := c.users[userID]; ok {
		return u, c.found[userID]
	}
	u, err := c.slack.UserInfo(c.ctx, userID)
	found = err == nil
	if !found {
		u = slackx.User{}
	}
	u.ID = userID
	if u.Name == "" {
		u.Name = userID
	}
	c.users[userID], c.found[userID] = u, found
	return u, found
}

// UserName implements markdown.SlackResolver: the display name of a
// mentioned user, or "" when Slack does not know them.
func (c *userCache) UserName(userID string) string {
	if u, found := c.get(userID); found {
		return u.Name
	}
	return ""
}
