package slackx

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"time"
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
	// HandleShortcut runs after forge-bot acknowledged the shortcut to
	// Slack. An error is reported to the member by the bot.
	HandleShortcut(ctx context.Context, sc ShortcutEvent) error
	// HandleSubmission returns messages keyed by input block ID for Slack
	// to show in the dialog, or nil to close it. It must return quickly:
	// forge-bot waits two and a half seconds for the answer.
	HandleSubmission(ctx context.Context, sub SubmissionEvent) map[string]string
}

// maxInteractionBody caps the envelope the bot may send.
const maxInteractionBody = 64 << 10

// shortcutTimeout bounds a shortcut handler. forge-bot waits ten seconds
// for the answer.
const shortcutTimeout = 10 * time.Second

// interaction is the envelope forge-bot posts for an interaction the
// forum owns (forge-bot service contract, version 1).
type interaction struct {
	Type       string `json:"type"`
	CallbackID string `json:"callback_id"`
	TriggerID  string `json:"trigger_id"`
	TeamID     string `json:"team_id"`
	User       struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"user"`
	Channel struct {
		ID string `json:"id"`
	} `json:"channel"`
	Message struct {
		TS       string `json:"ts"`
		ThreadTS string `json:"thread_ts"`
		Text     string `json:"text"`
		User     string `json:"user"`
		BotID    string `json:"bot_id"`
	} `json:"message"`
	View struct {
		CallbackID      string `json:"callback_id"`
		PrivateMetadata string `json:"private_metadata"`
		// Values maps block ID to action ID to what the member entered.
		Values map[string]map[string]struct {
			Value           string   `json:"value"`
			SelectedOptions []string `json:"selected_options"`
		} `json:"values"`
	} `json:"view"`
}

// Envelope types the forum acts on.
const (
	interactionMessageShortcut = "message_shortcut"
	interactionViewSubmission  = "view_submission"
)

// InteractionsHandler serves the endpoint forge-bot posts "Move to forum"
// interactions to. The bearer token is the only gate: the endpoint sits
// outside the forge-proxy identity check, and a request without exactly
// token gets a bare 401. An empty token accepts nothing.
//
// A shortcut runs h.HandleShortcut before answering: 200 when it
// succeeded, 500 when it failed, on which the bot tells the member. A
// dialog submission answers 200 with {"errors": {...}} for field errors
// and {} otherwise. An interaction from a workspace other than teamID, or
// one the forum does not own, answers 200 with {} and is ignored.
func InteractionsHandler(token, teamID string, h InteractionHandler, logger *slog.Logger) http.Handler {
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if token == "" || subtle.ConstantTimeCompare(got, want) != 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mediaType != "application/json" {
			w.WriteHeader(http.StatusUnsupportedMediaType)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxInteractionBody))
		if err != nil {
			status := http.StatusBadRequest
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				status = http.StatusRequestEntityTooLarge
			}
			w.WriteHeader(status)
			return
		}
		var in interaction
		if err := json.Unmarshal(body, &in); err != nil {
			logger.Warn("slack interaction could not be decoded", "err", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		// The values a member typed stay out of the log.
		if in.TeamID != teamID {
			logger.Warn("slack interaction from another workspace ignored", "team", in.TeamID, "type", in.Type)
			writeInteractionAnswer(w, logger, struct{}{})
			return
		}
		switch {
		case in.Type == interactionMessageShortcut && in.CallbackID == MoveShortcutCallbackID:
			sc := in.shortcut()
			// The bot answered Slack already and may stop waiting; the
			// dialog should open all the same.
			ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), shortcutTimeout)
			defer cancel()
			if err := h.HandleShortcut(ctx, sc); err != nil {
				logger.Error("move shortcut failed", "channel", sc.ChannelID, "err", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			writeInteractionAnswer(w, logger, struct{}{})

		case in.Type == interactionViewSubmission && in.CallbackID == MoveViewCallbackID:
			if fieldErrors := h.HandleSubmission(r.Context(), in.submission()); len(fieldErrors) > 0 {
				writeInteractionAnswer(w, logger, map[string]any{"errors": fieldErrors})
				return
			}
			writeInteractionAnswer(w, logger, struct{}{})

		default:
			logger.Info("slack interaction ignored", "type", in.Type, "callback_id", in.CallbackID)
			writeInteractionAnswer(w, logger, struct{}{})
		}
	})
}

// shortcut reads a message_shortcut envelope.
func (in interaction) shortcut() ShortcutEvent {
	threadTS := in.Message.ThreadTS
	if threadTS == in.Message.TS {
		// A thread root names itself as its thread.
		threadTS = ""
	}
	return ShortcutEvent{
		TriggerID:     in.TriggerID,
		TeamID:        in.TeamID,
		UserID:        in.User.ID,
		ChannelID:     in.Channel.ID,
		MessageTS:     in.Message.TS,
		ThreadTS:      threadTS,
		MessageText:   in.Message.Text,
		MessageUserID: in.Message.User,
		MessageBotID:  in.Message.BotID,
	}
}

// submission reads a view_submission envelope through the dialog's block
// and action IDs.
func (in interaction) submission() SubmissionEvent {
	sub := SubmissionEvent{
		TeamID:          in.TeamID,
		UserID:          in.User.ID,
		UserName:        in.User.Name,
		Title:           in.View.Values[MoveTitleBlockID][MoveTitleActionID].Value,
		PrivateMetadata: in.View.PrivateMetadata,
	}
	if tags := in.View.Values[MoveTagsBlockID][MoveTagsActionID].SelectedOptions; len(tags) > 0 {
		sub.Tags = append([]string(nil), tags...)
	}
	return sub
}

// writeInteractionAnswer answers 200 with v as JSON.
func writeInteractionAnswer(w http.ResponseWriter, logger *slog.Logger, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		logger.Warn("slack interaction answer not written", "err", err)
	}
}
