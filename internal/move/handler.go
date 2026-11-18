// Package move turns a Slack thread into a forum thread.
//
// A member picks the "Move to forum" shortcut on a message and confirms a
// title and tags in a dialog (handler.go). A background job then reads
// the thread and imports it in one transaction (job.go): the original
// post and the replies that answer it directly, as sorted by a Classifier
// (classifier.go, claude.go). Every other reply stays in Slack.
package move

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/forgeutah/forge-forum/internal/db"
	"github.com/forgeutah/forge-forum/internal/forum"
	"github.com/forgeutah/forge-forum/internal/markdown"
	"github.com/forgeutah/forge-forum/internal/slackx"
)

// What the bot tells a member whose move is refused.
const (
	notPublicText  = "Move to forum works on threads in public channels. This one is in a private channel or a direct message, so it stays in Slack."
	notMemberText  = "Move to forum works on threads a member started. This one was started by an app, so it stays in Slack."
	noTextText     = "Move to forum needs an original post with text. This one has none, so it stays in Slack."
	tooLongText    = "Move to forum needs an original post that fits on the forum. This one is too long, so it stays in Slack."
	alreadyMovedTo = "That thread is already on the forum."
)

// jobTimeout bounds one move, from reading the Slack thread to the import.
const jobTimeout = 3 * time.Minute

// Deps is what a Mover needs.
type Deps struct {
	Slack      slackx.Client
	Store      *forum.Store
	DB         *db.DB
	Classifier Classifier
	// BaseURL is the forum's public base URL, with no trailing slash.
	BaseURL string
	// TeamID is the configured Slack workspace. Interactions from any
	// other workspace are ignored.
	TeamID string
	// Logger defaults to slog.Default and Now to time.Now.
	Logger *slog.Logger
	Now    func() time.Time
	// Wake, when set, is called after a notification is enqueued so the
	// outbox worker delivers it without waiting for its next tick.
	Wake func()
}

// Mover handles the "Move to forum" shortcut and its dialog. It
// implements slackx.InteractionHandler.
type Mover struct {
	d Deps

	// jobsCtx is the parent of every job's context. It outlives the
	// interaction that starts a job and ends at Shutdown.
	jobsCtx  context.Context
	stopJobs context.CancelFunc

	jobs sync.WaitGroup
	mu   sync.Mutex
	// running holds the Slack threads with a move in progress, so a
	// redelivered submission does not start a second job.
	running map[string]bool
}

var _ slackx.InteractionHandler = (*Mover)(nil)

// New returns a Mover.
func New(d Deps) *Mover {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	d.BaseURL = strings.TrimRight(d.BaseURL, "/")
	jobsCtx, stopJobs := context.WithCancel(context.Background())
	return &Mover{d: d, running: map[string]bool{}, jobsCtx: jobsCtx, stopJobs: stopJobs}
}

// Shutdown cancels every move job in flight, and any started afterwards.
// A cancelled job imports nothing and leaves the mover's failure message
// in the outbox, to be delivered after the restart. Shutdown does not
// wait for the jobs; call Wait for that. It is safe to call more than
// once.
func (m *Mover) Shutdown() { m.stopJobs() }

// Wait blocks until every move job that was started has finished. Call it
// once no more submissions can arrive.
func (m *Mover) Wait() { m.jobs.Wait() }

// moveMetadata is the dialog's private metadata: the Slack thread the
// dialog was opened for.
type moveMetadata struct {
	Channel string `json:"channel"`
	TS      string `json:"ts"`
}

// HandleShortcut runs when a member picks "Move to forum" on a message.
// It does only quick checks before opening the dialog, because the
// trigger expires three seconds after the click: the channel must be
// public, the thread must not have been moved already, and its original
// post must have been written by a member and have text. A refusal is
// sent to the member as a direct message and no dialog opens.
func (m *Mover) HandleShortcut(ctx context.Context, sc slackx.ShortcutEvent) error {
	if sc.TeamID != m.d.TeamID {
		m.d.Logger.Warn("move shortcut from another workspace ignored", "team", sc.TeamID)
		return nil
	}
	// A shortcut on a reply moves the whole thread, from its root.
	onReply := sc.ThreadTS != "" && sc.ThreadTS != sc.MessageTS
	rootTS := sc.MessageTS
	if onReply {
		rootTS = sc.ThreadTS
	}

	_, refusal, err := m.refusal(ctx, sc.ChannelID, rootTS)
	if err != nil {
		return err
	}
	if refusal != "" {
		return m.tell(ctx, sc.UserID, refusal)
	}

	root, rootKnown := slackx.Message{User: sc.MessageUserID, BotID: sc.MessageBotID, Text: sc.MessageText}, true
	if onReply {
		// The title comes from the original post, which the shortcut on a
		// reply does not carry. Only that post is read: the trigger does
		// not wait for a long thread.
		msg, err := m.d.Slack.ThreadRoot(ctx, sc.ChannelID, rootTS)
		if err != nil {
			// Not worth losing the trigger over: the member types a title
			// and the job reads the thread again.
			m.d.Logger.Warn("move shortcut could not read the thread; opening with an empty title",
				"channel", sc.ChannelID, "err", err)
			root, rootKnown = slackx.Message{}, false
		} else {
			root = msg
		}
	}
	if rootKnown && !memberMessage(root) {
		return m.tell(ctx, sc.UserID, notMemberText)
	}
	if rootKnown && strings.TrimSpace(root.Text) == "" {
		// A file with no caption, say. The job checks the converted body.
		return m.tell(ctx, sc.UserID, noTextText)
	}

	tags, err := m.d.Store.ActiveTags(ctx)
	if err != nil {
		return err
	}
	meta, err := json.Marshal(moveMetadata{Channel: sc.ChannelID, TS: rootTS})
	if err != nil {
		return fmt.Errorf("encode move metadata: %w", err)
	}
	return m.d.Slack.OpenMoveDialog(ctx, sc.TriggerID, slackx.MoveDialog{
		Title:           titleFromPost(root.Text, newUserCache(ctx, m.d.Slack)),
		Tags:            tags,
		PrivateMetadata: string(meta),
	})
}

// HandleSubmission validates the dialog. It returns messages keyed by
// input block ID for Slack to show in the dialog, or nil to close it; a
// valid submission starts the move in the background.
func (m *Mover) HandleSubmission(ctx context.Context, sub slackx.SubmissionEvent) map[string]string {
	if sub.TeamID != m.d.TeamID {
		m.d.Logger.Warn("move submission from another workspace ignored", "team", sub.TeamID)
		return nil
	}
	fieldErrors := map[string]string{}

	title := strings.TrimSpace(sub.Title)
	var meta moveMetadata
	switch err := json.Unmarshal([]byte(sub.PrivateMetadata), &meta); {
	case err != nil || meta.Channel == "" || meta.TS == "":
		// There is no block for this; the title is the first thing they see.
		m.d.Logger.Error("move submission carried unusable metadata", "err", err)
		fieldErrors[slackx.MoveTitleBlockID] = "This dialog lost track of its thread. Close it and pick Move to forum again."
	case title == "":
		fieldErrors[slackx.MoveTitleBlockID] = "Give the thread a title."
	case utf8.RuneCountInString(title) > forum.MaxTitleLen:
		fieldErrors[slackx.MoveTitleBlockID] = fmt.Sprintf("Keep the title to %d characters or fewer.", forum.MaxTitleLen)
	}

	tags, tagProblem := m.checkTags(ctx, sub.Tags)
	if tagProblem != "" {
		fieldErrors[slackx.MoveTagsBlockID] = tagProblem
	}
	if len(fieldErrors) > 0 {
		return fieldErrors
	}

	key := slackx.ThreadKey(meta.Channel, meta.TS)
	m.mu.Lock()
	if m.running[key] {
		m.mu.Unlock()
		return nil // a redelivered submission; the first one is at work
	}
	m.running[key] = true
	m.mu.Unlock()

	j := job{
		channelID: meta.Channel,
		rootTS:    meta.TS,
		title:     title,
		tags:      tags,
		moverID:   sub.UserID,
		moverName: sub.UserName,
	}
	// The job outlives the interaction that started it, but not the process.
	jobCtx, cancel := context.WithTimeout(m.jobsCtx, jobTimeout)
	m.jobs.Go(func() {
		defer cancel()
		defer func() {
			m.mu.Lock()
			delete(m.running, key)
			m.mu.Unlock()
		}()
		m.run(jobCtx, j)
	})
	return nil
}

// checkTags returns the selected tags, trimmed and without repeats, or
// the message to show under the tag input.
func (m *Mover) checkTags(ctx context.Context, selected []string) ([]string, string) {
	active, err := m.d.Store.ActiveTags(ctx)
	if err != nil {
		m.d.Logger.Error("move submission could not load tags", "err", err)
		return nil, "We could not load the tag list. Try again."
	}
	isActive := make(map[string]bool, len(active))
	for _, slug := range active {
		isActive[slug] = true
	}
	var tags []string
	seen := map[string]bool{}
	for _, slug := range selected {
		slug = strings.TrimSpace(slug)
		if slug == "" || seen[slug] {
			continue
		}
		if !isActive[slug] {
			return nil, "One of those tags is no longer on the list. Pick from the tags shown."
		}
		seen[slug] = true
		tags = append(tags, slug)
	}
	if len(tags) == 0 {
		return nil, "Pick at least one tag."
	}
	return tags, ""
}

// refusal returns what to tell a member whose thread cannot be moved, or
// "" when it can: the channel must be public and the thread not moved
// yet. It also returns the channel, for its name.
func (m *Mover) refusal(ctx context.Context, channelID, rootTS string) (slackx.Channel, string, error) {
	ch, err := m.d.Slack.ChannelInfo(ctx, channelID)
	switch {
	case slackx.ErrorCode(err) == slackx.ErrChannelNotFound:
		// How Slack answers for a private channel or DM the bot is not in.
		return ch, notPublicText, nil
	case err != nil:
		return ch, "", fmt.Errorf("read channel: %w", err)
	case !ch.IsPublic:
		return ch, notPublicText, nil
	}
	id, moved, err := m.d.Store.SlackThreadID(ctx, channelID, rootTS)
	if err != nil {
		return ch, "", err
	}
	if moved {
		return ch, m.alreadyMovedText(id), nil
	}
	return ch, "", nil
}

// bodyRefusal returns what to tell a member whose thread's original post
// cannot become a thread body, or "" when it can. The forum would reject
// such a body at import, however often the move is tried.
func bodyRefusal(bodyMD string) string {
	body := strings.TrimSpace(bodyMD)
	switch {
	case body == "":
		return noTextText
	case utf8.RuneCountInString(body) > forum.MaxBodyLen:
		return tooLongText
	}
	return ""
}

// alreadyMovedText points a member at the forum thread a Slack thread
// already became.
func (m *Mover) alreadyMovedText(threadID int64) string {
	return alreadyMovedTo + "\n<" + m.threadURL(threadID) + "|Open it on the forum →>"
}

func (m *Mover) threadURL(threadID int64) string {
	return m.d.BaseURL + forum.ThreadPath(threadID)
}

// tell sends a member a direct message from the bot.
func (m *Mover) tell(ctx context.Context, userID, text string) error {
	if err := m.d.Slack.PostMessage(ctx, userID, text, ""); err != nil {
		return fmt.Errorf("message member: %w", err)
	}
	return nil
}

// memberMessage reports whether a Slack message is an ordinary post by a
// person. App and bot posts, and system messages such as "joined the
// channel", are not. A reply also sent to the channel (thread_broadcast)
// and a post with a file attached (file_share) are ordinary posts.
func memberMessage(msg slackx.Message) bool {
	if msg.User == "" || msg.BotID != "" {
		return false
	}
	switch msg.SubType {
	case "", "thread_broadcast", "file_share":
		return true
	}
	return false
}

var (
	titleReference = regexp.MustCompile(`<([^<>\n]+)>`)
	// One pattern per Slack emphasis marker: the marker must open at the
	// start of a word and close at the end of one, as in Slack.
	titleEmphasis = []*regexp.Regexp{
		regexp.MustCompile(`(^|[^\pL\pN])\*(\S(?:[^*]*\S)?)\*([^\pL\pN]|$)`),
		regexp.MustCompile(`(^|[^\pL\pN])_(\S(?:[^_]*\S)?)_([^\pL\pN]|$)`),
		regexp.MustCompile(`(^|[^\pL\pN])~(\S(?:[^~]*\S)?)~([^\pL\pN]|$)`),
		regexp.MustCompile("(^|[^\\pL\\pN])`([^`]+)`([^\\pL\\pN]|$)"),
	}
)

// titleLine returns the first line of a Slack post that has words on it,
// without the quote marker, bullet, or code fence that may lead it.
func titleLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		for stripped := true; stripped; {
			stripped = false
			for _, lead := range []string{"&gt;", "•", "```", "- "} {
				if rest, ok := strings.CutPrefix(line, lead); ok {
					line, stripped = strings.TrimSpace(rest), true
				}
			}
		}
		if line != "" {
			return line
		}
	}
	return ""
}

// titleFromPost suggests a thread title: the first line of a Slack post
// as plain text, capped at the dialog's limit. The member edits it.
func titleFromPost(text string, users *userCache) string {
	line := titleLine(text)
	line = titleReference.ReplaceAllStringFunc(line, func(ref string) string {
		target, label, _ := strings.Cut(ref[1:len(ref)-1], "|")
		switch {
		case strings.HasPrefix(target, "@"):
			if name := users.UserName(target[1:]); name != "" {
				return "@" + name
			}
			return strings.TrimPrefix(label, "@")
		case strings.HasPrefix(target, "#"):
			return "#" + strings.TrimPrefix(label, "#")
		case strings.HasPrefix(target, "!"):
			if label != "" {
				return label
			}
			command, _, _ := strings.Cut(target[1:], "^")
			return "@" + command
		case label != "":
			return label
		}
		return target
	})
	for _, marker := range titleEmphasis {
		// Each match consumes its neighbours, so adjacent runs need a
		// second pass.
		for range 4 {
			stripped := marker.ReplaceAllString(line, "$1$2$3")
			if stripped == line {
				break
			}
			line = stripped
		}
	}
	line = strings.Join(strings.Fields(markdown.UnescapeSlack(line)), " ")

	if r := []rune(line); len(r) > slackx.MoveTitleMaxLen {
		line = strings.TrimSpace(string(r[:slackx.MoveTitleMaxLen]))
	}
	return line
}
