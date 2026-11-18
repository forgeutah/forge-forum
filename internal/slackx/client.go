// Package slackx is the forum's view of Slack.
//
// Client names the handful of Web API calls the forum makes, so the rest
// of the code never touches slack-go directly and tests can use Fake
// (plan KTD5). RealClient is the slack-go implementation.
package slackx

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/slack-go/slack"
)

// Client is everything the forum asks of Slack. The real implementation
// wraps slack-go; tests use Fake.
type Client interface {
	// PostMessage posts text (Slack mrkdwn) to a channel ID, or to a user
	// ID to send a direct message. threadTS, when non-empty, posts into
	// that thread (it must be the thread root ts).
	PostMessage(ctx context.Context, channel, text, threadTS string) error
	// ThreadReplies returns every message of the thread containing ts,
	// root first, following pagination.
	ThreadReplies(ctx context.Context, channel, ts string) ([]Message, error)
	ChannelInfo(ctx context.Context, channel string) (Channel, error)
	UserInfo(ctx context.Context, userID string) (User, error)
	Permalink(ctx context.Context, channel, ts string) (string, error)
	// OpenMoveDialog opens the "Move to forum" modal for a shortcut trigger.
	OpenMoveDialog(ctx context.Context, triggerID string, d MoveDialog) error
}

// Message is one Slack message in a thread.
type Message struct {
	User     string
	BotID    string
	Text     string
	TS       string
	ThreadTS string
	SubType  string
}

// Channel describes a conversation. IsPublic is true only for a public
// channel: not private, not a DM, not a group DM.
type Channel struct {
	ID       string
	Name     string
	IsPublic bool
}

// User describes a Slack user. Name is the display name, falling back to
// the real name.
type User struct {
	ID        string
	Name      string
	AvatarURL string
	IsBot     bool
}

// MoveDialog is the content of the "Move to forum" modal.
type MoveDialog struct {
	Title           string   // pre-filled, editable
	Tags            []string // available tag slugs, multi-select, at least one required
	PrivateMetadata string   // opaque; round-trips to the submission
}

// Stable IDs the Socket Mode handler reads from the shortcut and the
// modal submission.
const (
	MoveShortcutCallbackID = "move_to_forum"
	MoveViewCallbackID     = "move_to_forum_submit"
	MoveTitleBlockID       = "title"
	MoveTitleActionID      = "title_input"
	MoveTagsBlockID        = "tags"
	MoveTagsActionID       = "tags_select"
)

// MoveTitleMaxLen is the longest thread title the modal accepts.
const MoveTitleMaxLen = 200

// moveDialogNote is the line under the modal's inputs.
const moveDialogNote = "The original post and the replies that answer it directly move to the forum under their authors' names. Other replies stay here in Slack."

// repliesPageSize is the conversations.replies page size.
const repliesPageSize = 200

// APIError is an error Slack reported by code, such as "channel_not_found".
type APIError struct {
	Code string
}

func (e *APIError) Error() string { return "slack: " + e.Code }

// ErrorCode returns the Slack error code carried by err, or "" when err
// is not a Slack API error.
func ErrorCode(err error) string {
	var api *APIError
	if errors.As(err, &api) {
		return api.Code
	}
	return ""
}

// RateLimitedError means Slack asked the forum to slow down. RetryAfter
// is how long Slack said to wait; zero when it did not say.
type RateLimitedError struct {
	RetryAfter time.Duration
}

func (e *RateLimitedError) Error() string {
	return fmt.Sprintf("slack: rate limited, retry after %s", e.RetryAfter)
}

// RealClient talks to Slack's Web API through slack-go.
type RealClient struct {
	api *slack.Client
}

var _ Client = (*RealClient)(nil)

// NewClient returns the real client. baseURL "" means Slack's API; tests
// pass an httptest server URL. httpClient nil means a client with a 30
// second timeout.
func NewClient(botToken, baseURL string, httpClient *http.Client) *RealClient {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	opts := []slack.Option{slack.OptionHTTPClient(httpClient)}
	if baseURL != "" {
		// slack-go appends the method name directly to the endpoint.
		opts = append(opts, slack.OptionAPIURL(strings.TrimRight(baseURL, "/")+"/"))
	}
	return &RealClient{api: slack.New(botToken, opts...)}
}

// API returns the underlying slack-go client, which Socket Mode needs.
func (c *RealClient) API() *slack.Client { return c.api }

// PostMessage implements Client.
func (c *RealClient) PostMessage(ctx context.Context, channel, text, threadTS string) error {
	opts := []slack.MsgOption{
		slack.MsgOptionText(text, false),
		slack.MsgOptionDisableLinkUnfurl(),
		slack.MsgOptionDisableMediaUnfurl(),
	}
	if threadTS != "" {
		opts = append(opts, slack.MsgOptionTS(threadTS))
	}
	_, _, err := c.api.PostMessageContext(ctx, channel, opts...)
	return mapErr(err)
}

// ThreadReplies implements Client. When the bot is not in the channel it
// joins once and retries; Slack only allows that for public channels.
func (c *RealClient) ThreadReplies(ctx context.Context, channel, ts string) ([]Message, error) {
	msgs, err := c.threadReplies(ctx, channel, ts)
	if ErrorCode(err) != "not_in_channel" {
		return msgs, err
	}
	if _, _, _, jerr := c.api.JoinConversationContext(ctx, channel); jerr != nil {
		return nil, fmt.Errorf("join channel after not_in_channel: %w", mapErr(jerr))
	}
	return c.threadReplies(ctx, channel, ts)
}

func (c *RealClient) threadReplies(ctx context.Context, channel, ts string) ([]Message, error) {
	var out []Message
	cursor := ""
	for {
		page, _, next, err := c.api.GetConversationRepliesContext(ctx, &slack.GetConversationRepliesParameters{
			ChannelID: channel,
			Timestamp: ts,
			Cursor:    cursor,
			Limit:     repliesPageSize,
		})
		if err != nil {
			return nil, mapErr(err)
		}
		// Asked about a reply, Slack returns only that reply. Start over
		// from the thread root it names.
		if cursor == "" && len(page) > 0 {
			if root := page[0].ThreadTimestamp; root != "" && root != ts && page[0].Timestamp == ts {
				ts = root
				continue
			}
		}
		for _, m := range page {
			out = append(out, Message{
				User:     m.User,
				BotID:    m.BotID,
				Text:     m.Text,
				TS:       m.Timestamp,
				ThreadTS: m.ThreadTimestamp,
				SubType:  m.SubType,
			})
		}
		if next == "" {
			return out, nil
		}
		cursor = next
	}
}

// ChannelInfo implements Client.
func (c *RealClient) ChannelInfo(ctx context.Context, channel string) (Channel, error) {
	ch, err := c.api.GetConversationInfoContext(ctx, &slack.GetConversationInfoInput{ChannelID: channel})
	if err != nil {
		return Channel{}, mapErr(err)
	}
	return Channel{
		ID:       ch.ID,
		Name:     ch.Name,
		IsPublic: ch.IsChannel && !ch.IsPrivate && !ch.IsIM && !ch.IsMpIM,
	}, nil
}

// UserInfo implements Client.
func (c *RealClient) UserInfo(ctx context.Context, userID string) (User, error) {
	u, err := c.api.GetUserInfoContext(ctx, userID)
	if err != nil {
		return User{}, mapErr(err)
	}
	name := strings.TrimSpace(u.Profile.DisplayName)
	for _, fallback := range []string{u.Profile.RealName, u.RealName, u.Name} {
		if name != "" {
			break
		}
		name = strings.TrimSpace(fallback)
	}
	return User{ID: u.ID, Name: name, AvatarURL: u.Profile.Image72, IsBot: u.IsBot}, nil
}

// Permalink implements Client.
func (c *RealClient) Permalink(ctx context.Context, channel, ts string) (string, error) {
	link, err := c.api.GetPermalinkContext(ctx, &slack.PermalinkParameters{Channel: channel, Ts: ts})
	return link, mapErr(err)
}

// OpenMoveDialog implements Client.
func (c *RealClient) OpenMoveDialog(ctx context.Context, triggerID string, d MoveDialog) error {
	if len(d.Tags) == 0 {
		return errors.New("slackx: move dialog needs at least one tag")
	}
	_, err := c.api.OpenViewContext(ctx, triggerID, moveView(d))
	return mapErr(err)
}

// moveView builds the "Move to forum" modal.
func moveView(d MoveDialog) slack.ModalViewRequest {
	plain := func(s string) *slack.TextBlockObject {
		return slack.NewTextBlockObject(slack.PlainTextType, s, false, false)
	}

	title := slack.NewPlainTextInputBlockElement(nil, MoveTitleActionID)
	// Slack rejects an initial value longer than the maximum.
	title.InitialValue = truncateRunes(d.Title, MoveTitleMaxLen)
	title.MaxLength = MoveTitleMaxLen

	options := make([]*slack.OptionBlockObject, 0, len(d.Tags))
	for _, tag := range d.Tags {
		options = append(options, slack.NewOptionBlockObject(tag, plain(tag), nil))
	}
	tags := slack.NewOptionsMultiSelectBlockElement(slack.MultiOptTypeStatic, plain("Choose tags"), MoveTagsActionID, options...)

	return slack.ModalViewRequest{
		Type:            slack.VTModal,
		CallbackID:      MoveViewCallbackID,
		Title:           plain("Move to forum"),
		Submit:          plain("Move thread"),
		Close:           plain("Cancel"),
		PrivateMetadata: d.PrivateMetadata,
		Blocks: slack.Blocks{BlockSet: []slack.Block{
			// Input blocks are required unless marked optional, which is
			// what makes at least one tag mandatory.
			slack.NewInputBlock(MoveTitleBlockID, plain("Thread title"), nil, title),
			slack.NewInputBlock(MoveTagsBlockID, plain("Tags"), nil, tags),
			slack.NewContextBlock("", plain(moveDialogNote)),
		}},
	}
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

// mapErr turns slack-go's errors into this package's types so callers do
// not import slack-go to classify a failure.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	var limited *slack.RateLimitedError
	if errors.As(err, &limited) {
		return &RateLimitedError{RetryAfter: limited.RetryAfter}
	}
	var status slack.StatusCodeError
	if errors.As(err, &status) && status.Code == http.StatusTooManyRequests {
		return &RateLimitedError{}
	}
	var resp slack.SlackErrorResponse
	if errors.As(err, &resp) {
		if resp.Err == "ratelimited" || resp.Err == "rate_limited" {
			return &RateLimitedError{}
		}
		return &APIError{Code: resp.Err}
	}
	return err
}
