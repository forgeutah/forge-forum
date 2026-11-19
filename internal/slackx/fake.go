package slackx

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// Fake is an in-memory Client for tests. Script results by filling the
// exported fields before use; read what happened from Posted, Dialogs
// and Calls. Methods are safe for concurrent use; while other goroutines
// may still be calling, read through PostedMessages rather than the
// fields.
type Fake struct {
	mu sync.Mutex

	// Threads holds thread messages, root first, keyed by ThreadKey.
	Threads map[string][]Message
	// Channels and Users are keyed by ID. A missing entry is reported as
	// Slack would: ErrChannelNotFound or ErrUserNotFound.
	Channels map[string]Channel
	Users    map[string]User
	// Permalinks is keyed by ThreadKey(channel, ts). A missing entry gets
	// a made-up but stable link.
	Permalinks map[string]string

	// PostErrs is consumed one entry per PostMessage call; a nil entry,
	// or an empty list, means the post succeeds.
	PostErrs []error
	// DialogErr, when set, is returned by every OpenMoveDialog call.
	DialogErr error

	// Posted holds the messages that were accepted, in order. Calls that
	// were answered with an error from PostErrs are not in it.
	Posted []PostedMessage
	// Dialogs holds the dialogs that were opened, in order.
	Dialogs []OpenedDialog
	// Calls holds every method call, including failed ones, in order.
	Calls []Call
}

// PostedMessage is one message the Fake accepted.
type PostedMessage struct {
	Channel  string
	Text     string
	ThreadTS string
}

// OpenedDialog is one dialog the Fake opened.
type OpenedDialog struct {
	TriggerID string
	Dialog    MoveDialog
}

// Call is one recorded method call. Args are the string arguments in
// declaration order; OpenMoveDialog records only the trigger ID.
type Call struct {
	Method string
	Args   []string
}

var _ Client = (*Fake)(nil)

// ThreadKey is the map key for Fake.Threads and Fake.Permalinks.
func ThreadKey(channel, ts string) string { return channel + ":" + ts }

// PostedMessages returns a copy of Posted, safe to call while other
// goroutines are still posting.
func (f *Fake) PostedMessages() []PostedMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]PostedMessage(nil), f.Posted...)
}

func (f *Fake) record(method string, args ...string) {
	f.Calls = append(f.Calls, Call{Method: method, Args: args})
}

// PostMessage implements Client.
func (f *Fake) PostMessage(ctx context.Context, channel, text, threadTS string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("PostMessage", channel, text, threadTS)
	if len(f.PostErrs) > 0 {
		err := f.PostErrs[0]
		f.PostErrs = f.PostErrs[1:]
		if err != nil {
			return err
		}
	}
	f.Posted = append(f.Posted, PostedMessage{Channel: channel, Text: text, ThreadTS: threadTS})
	return nil
}

// ThreadReplies implements Client. ts may be the root or any reply in a
// scripted thread.
func (f *Fake) ThreadReplies(ctx context.Context, channel, ts string) ([]Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("ThreadReplies", channel, ts)
	msgs, err := f.thread(channel, ts)
	if err != nil {
		return nil, err
	}
	return append([]Message(nil), msgs...), nil
}

// ThreadRoot implements Client. ts may be the root or any reply in a
// scripted thread.
func (f *Fake) ThreadRoot(ctx context.Context, channel, ts string) (Message, error) {
	if err := ctx.Err(); err != nil {
		return Message{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("ThreadRoot", channel, ts)
	msgs, err := f.thread(channel, ts)
	if err != nil {
		return Message{}, err
	}
	if len(msgs) == 0 {
		return Message{}, &APIError{Code: ErrThreadNotFound}
	}
	return msgs[0], nil
}

// thread returns the scripted thread in channel that ts is the root of
// or a message in. The caller holds f.mu.
func (f *Fake) thread(channel, ts string) ([]Message, error) {
	if msgs, ok := f.Threads[ThreadKey(channel, ts)]; ok {
		return msgs, nil
	}
	prefix := channel + ":"
	for key, msgs := range f.Threads {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		for _, m := range msgs {
			if m.TS == ts {
				return msgs, nil
			}
		}
	}
	return nil, &APIError{Code: ErrThreadNotFound}
}

// ChannelInfo implements Client.
func (f *Fake) ChannelInfo(ctx context.Context, channel string) (Channel, error) {
	if err := ctx.Err(); err != nil {
		return Channel{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("ChannelInfo", channel)
	ch, ok := f.Channels[channel]
	if !ok {
		return Channel{}, &APIError{Code: ErrChannelNotFound}
	}
	return ch, nil
}

// UserInfo implements Client.
func (f *Fake) UserInfo(ctx context.Context, userID string) (User, error) {
	if err := ctx.Err(); err != nil {
		return User{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("UserInfo", userID)
	u, ok := f.Users[userID]
	if !ok {
		return User{}, &APIError{Code: ErrUserNotFound}
	}
	return u, nil
}

// Permalink implements Client.
func (f *Fake) Permalink(ctx context.Context, channel, ts string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Permalink", channel, ts)
	if link, ok := f.Permalinks[ThreadKey(channel, ts)]; ok {
		return link, nil
	}
	return fmt.Sprintf("https://slack.test/archives/%s/p%s", channel, strings.ReplaceAll(ts, ".", "")), nil
}

// OpenMoveDialog implements Client.
func (f *Fake) OpenMoveDialog(ctx context.Context, triggerID string, d MoveDialog) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("OpenMoveDialog", triggerID)
	if f.DialogErr != nil {
		return f.DialogErr
	}
	d.Tags = append([]string(nil), d.Tags...)
	f.Dialogs = append(f.Dialogs, OpenedDialog{TriggerID: triggerID, Dialog: d})
	return nil
}
