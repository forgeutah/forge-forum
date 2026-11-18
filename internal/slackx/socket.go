package slackx

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
)

// ShortcutEvent is a member picking "Move to forum" on a Slack message.
type ShortcutEvent struct {
	// TriggerID opens the dialog; Slack expires it after three seconds.
	TriggerID string
	TeamID    string
	// UserID is the member who picked the shortcut.
	UserID    string
	ChannelID string
	// MessageTS is the message the shortcut was invoked on. ThreadTS is
	// its thread root, empty when the message is not in a thread.
	MessageTS string
	ThreadTS  string
	// MessageText, MessageUserID and MessageBotID describe that message.
	MessageText   string
	MessageUserID string
	MessageBotID  string
}

// SubmissionEvent is a member submitting the "Move to forum" dialog.
type SubmissionEvent struct {
	TeamID string
	UserID string
	// UserName is the member's Slack handle, a fallback for when their
	// profile cannot be read.
	UserName        string
	Title           string
	Tags            []string
	PrivateMetadata string
}

// InteractionHandler receives "Move to forum" interactions. move.Mover
// implements it.
type InteractionHandler interface {
	// HandleShortcut runs after the shortcut was acknowledged.
	HandleShortcut(ctx context.Context, sc ShortcutEvent) error
	// HandleSubmission returns messages keyed by input block ID for Slack
	// to show in the dialog, or nil to close it. It must return quickly:
	// Slack waits three seconds for the acknowledgement.
	HandleSubmission(ctx context.Context, sub SubmissionEvent) map[string]string
}

// interactionKind says what an interactive payload is to the forum.
type interactionKind int

const (
	interactionIgnored interactionKind = iota
	interactionShortcut
	interactionSubmission
)

// decodeInteraction translates a Slack interactive payload. Only the
// "Move to forum" message shortcut and its dialog submission are
// recognised; everything else is interactionIgnored.
func decodeInteraction(cb slack.InteractionCallback) (interactionKind, ShortcutEvent, SubmissionEvent) {
	teamID := cb.Team.ID
	if teamID == "" {
		teamID = cb.User.TeamID
	}
	switch {
	case cb.Type == slack.InteractionTypeMessageAction && cb.CallbackID == MoveShortcutCallbackID:
		messageTS := cb.Message.Timestamp
		if messageTS == "" {
			messageTS = cb.MessageTs
		}
		threadTS := cb.Message.ThreadTimestamp
		if threadTS == messageTS {
			// A thread root names itself as its thread.
			threadTS = ""
		}
		return interactionShortcut, ShortcutEvent{
			TriggerID:     cb.TriggerID,
			TeamID:        teamID,
			UserID:        cb.User.ID,
			ChannelID:     cb.Channel.ID,
			MessageTS:     messageTS,
			ThreadTS:      threadTS,
			MessageText:   cb.Message.Text,
			MessageUserID: cb.Message.User,
			MessageBotID:  cb.Message.BotID,
		}, SubmissionEvent{}

	case cb.Type == slack.InteractionTypeViewSubmission && cb.View.CallbackID == MoveViewCallbackID:
		sub := SubmissionEvent{
			TeamID:          teamID,
			UserID:          cb.User.ID,
			UserName:        cb.User.Name,
			PrivateMetadata: cb.View.PrivateMetadata,
		}
		if cb.View.State != nil {
			values := cb.View.State.Values
			sub.Title = values[MoveTitleBlockID][MoveTitleActionID].Value
			for _, opt := range values[MoveTagsBlockID][MoveTagsActionID].SelectedOptions {
				sub.Tags = append(sub.Tags, opt.Value)
			}
		}
		return interactionSubmission, ShortcutEvent{}, sub
	}
	return interactionIgnored, ShortcutEvent{}, SubmissionEvent{}
}

// dispatchInteraction acknowledges one interactive payload and hands it
// to h. ack sends the acknowledgement; a nil payload is an empty ack.
//
// A shortcut is acknowledged before any work. A submission is
// acknowledged with the handler's field errors, which is how Slack shows
// them in the dialog, or with an empty ack, which closes it.
func dispatchInteraction(ctx context.Context, cb slack.InteractionCallback, h InteractionHandler, ack func(payload any), logger *slog.Logger) {
	kind, sc, sub := decodeInteraction(cb)
	switch kind {
	case interactionShortcut:
		ack(nil)
		if err := h.HandleShortcut(ctx, sc); err != nil {
			logger.Error("move shortcut failed", "channel", sc.ChannelID, "err", err)
		}
	case interactionSubmission:
		if fieldErrors := h.HandleSubmission(ctx, sub); len(fieldErrors) > 0 {
			ack(slack.NewErrorsViewSubmissionResponse(fieldErrors))
			return
		}
		ack(nil)
	default:
		ack(nil)
	}
}

// RunSocketMode connects to Slack over Socket Mode with the app-level
// token and dispatches "Move to forum" interactions to h until ctx is
// done. Every envelope is acknowledged; events the forum does not handle
// are acknowledged and ignored.
//
// Reconnecting is slack-go's job. RunSocketMode returns nil once ctx is
// cancelled, and an error when Slack rejects the app-level token, which
// retrying cannot fix.
func RunSocketMode(ctx context.Context, c *RealClient, appToken string, h InteractionHandler, logger *slog.Logger) error {
	// Socket Mode opens its connection with the app-level token, which the
	// Web API client was built without. Work on a copy so that client is
	// left as it was.
	api := *c.API()
	slack.OptionAppLevelToken(appToken)(&api)
	sm := socketmode.New(&api)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- sm.RunContext(ctx) }()

	// Handlers run off the event loop so one slow Slack call never delays
	// the acknowledgement of the next envelope.
	var handlers sync.WaitGroup
	defer handlers.Wait()

	for {
		select {
		case err := <-done:
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("slack socket mode: %w", err)

		case evt := <-sm.Events:
			switch evt.Type {
			case socketmode.EventTypeConnected:
				logger.Info("slack socket mode connected")
			case socketmode.EventTypeConnectionError, socketmode.EventTypeInvalidAuth, socketmode.EventTypeIncomingError:
				// The event data can carry Slack payloads; log only the kind.
				logger.Warn("slack socket mode problem", "event", string(evt.Type))
			}
			if evt.Request == nil || evt.Request.EnvelopeID == "" {
				continue // connection lifecycle events carry no envelope
			}
			req := *evt.Request
			ack := func(payload any) {
				if err := sm.AckCtx(ctx, req.EnvelopeID, payload); err != nil {
					logger.Error("slack acknowledgement failed", "err", err)
				}
			}
			cb, ok := evt.Data.(slack.InteractionCallback)
			if evt.Type != socketmode.EventTypeInteractive || !ok {
				ack(nil)
				continue
			}
			handlers.Go(func() { dispatchInteraction(ctx, cb, h, ack, logger) })
		}
	}
}
