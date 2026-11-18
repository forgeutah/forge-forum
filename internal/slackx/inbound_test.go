package slackx

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

const (
	inboundToken = "forum-service-token-0123456789abcdef"
	inboundTeam  = "T0FORGE"
)

// Envelopes as the forge-bot service contract (version 1) describes them.
const (
	shortcutOnRootEnvelope = `{
		"type": "message_shortcut",
		"callback_id": "move_to_forum",
		"trigger_id": "9001.43.abcdef",
		"team_id": "T0FORGE",
		"user": {"id": "UMOVER", "name": "mo"},
		"channel": {"id": "C1"},
		"message": {"ts": "1760950800.000100", "thread_ts": "", "user": "UASKER", "bot_id": "", "subtype": "",
			"text": "Has anyone moved from Ingress to *Gateway API*?"}
	}`

	shortcutOnReplyEnvelope = `{
		"type": "message_shortcut",
		"callback_id": "move_to_forum",
		"trigger_id": "9001.42.abcdef",
		"team_id": "T0FORGE",
		"user": {"id": "UMOVER", "name": "mo"},
		"channel": {"id": "C1"},
		"message": {"ts": "1760950860.000200", "thread_ts": "1760950800.000100", "user": "UR1", "bot_id": "", "subtype": "",
			"text": "Yes, <@UASKER> we did it last quarter."}
	}`

	submissionEnvelope = `{
		"type": "view_submission",
		"callback_id": "move_to_forum_submit",
		"trigger_id": "9002.44.abcdef",
		"team_id": "T0FORGE",
		"user": {"id": "UMOVER", "name": "mo"},
		"view": {
			"id": "V1",
			"callback_id": "move_to_forum_submit",
			"private_metadata": "{\"channel\":\"C1\",\"ts\":\"1760950800.000100\"}",
			"values": {
				"title": {"title_input": {"type": "plain_text_input", "value": "Rolling out Gateway API"}},
				"tags": {"tags_select": {"type": "multi_static_select", "selected_options": ["kubernetes", "architecture"]}}
			}
		}
	}`
)

// recordingHandler records the interactions it is handed.
type recordingHandler struct {
	calls       []string
	shortcut    ShortcutEvent
	submission  SubmissionEvent
	shortcutErr error
	fieldErrors map[string]string
}

func (h *recordingHandler) HandleShortcut(_ context.Context, sc ShortcutEvent) error {
	h.calls = append(h.calls, "shortcut")
	h.shortcut = sc
	return h.shortcutErr
}

func (h *recordingHandler) HandleSubmission(_ context.Context, sub SubmissionEvent) map[string]string {
	h.calls = append(h.calls, "submission")
	h.submission = sub
	return h.fieldErrors
}

// interactionRequest is a request shaped as the bot sends it.
func interactionRequest(body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/internal/slack/interactions", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+inboundToken)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Forge-Bot-Contract", "1")
	return r
}

// serveInteraction runs r through a handler built with the test token and team.
func serveInteraction(h InteractionHandler, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	InteractionsHandler(inboundToken, inboundTeam, h, logger).ServeHTTP(w, r)
	return w
}

// TestInteractionsHandler_ServeHTTP_BadBearer_Unauthorized checks a missing or wrong token gets a bare 401 and the handler is not called.
func TestInteractionsHandler_ServeHTTP_BadBearer_Unauthorized(t *testing.T) {
	headers := map[string]string{
		"missing":             "",
		"wrong token":         "Bearer " + strings.Repeat("x", len(inboundToken)),
		"token prefix":        "Bearer " + inboundToken[:len(inboundToken)-1],
		"token with a suffix": "Bearer " + inboundToken + "x",
		"not a bearer scheme": "Basic " + inboundToken,
		"bare token":          inboundToken,
		"empty bearer":        "Bearer ",
	}
	for name, header := range headers {
		t.Run(name, func(t *testing.T) {
			h := &recordingHandler{}
			r := interactionRequest(shortcutOnRootEnvelope)
			r.Header.Del("Authorization")
			if header != "" {
				r.Header.Set("Authorization", header)
			}
			w := serveInteraction(h, r)
			if w.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", w.Code)
			}
			if body := strings.TrimSpace(w.Body.String()); body != "" {
				t.Errorf("body = %q, want no detail", body)
			}
			if len(h.calls) != 0 {
				t.Errorf("handler called: %v", h.calls)
			}
		})
	}
}

// TestInteractionsHandler_ServeHTTP_NoTokenConfigured_Unauthorized checks a handler built with an empty token accepts nothing.
func TestInteractionsHandler_ServeHTTP_NoTokenConfigured_Unauthorized(t *testing.T) {
	h := &recordingHandler{}
	for _, header := range []string{"", "Bearer ", "Bearer"} {
		r := interactionRequest(shortcutOnRootEnvelope)
		r.Header.Del("Authorization")
		if header != "" {
			r.Header.Set("Authorization", header)
		}
		w := httptest.NewRecorder()
		InteractionsHandler("", inboundTeam, h, slog.New(slog.NewTextHandler(io.Discard, nil))).ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized || len(h.calls) != 0 {
			t.Errorf("Authorization %q: status = %d, calls = %v; want 401 and no call", header, w.Code, h.calls)
		}
	}
}

// TestInteractionsHandler_ServeHTTP_WrongContentType_Rejected checks only application/json is accepted.
func TestInteractionsHandler_ServeHTTP_WrongContentType_Rejected(t *testing.T) {
	for _, ct := range []string{"", "application/x-www-form-urlencoded", "text/plain", "application/jsonx"} {
		h := &recordingHandler{}
		r := interactionRequest(shortcutOnRootEnvelope)
		r.Header.Del("Content-Type")
		if ct != "" {
			r.Header.Set("Content-Type", ct)
		}
		if w := serveInteraction(h, r); w.Code != http.StatusUnsupportedMediaType || len(h.calls) != 0 {
			t.Errorf("Content-Type %q: status = %d, calls = %v; want 415 and no call", ct, w.Code, h.calls)
		}
	}

	h := &recordingHandler{}
	r := interactionRequest(shortcutOnRootEnvelope)
	r.Header.Set("Content-Type", "application/json; charset=utf-8")
	if w := serveInteraction(h, r); w.Code != http.StatusOK || len(h.calls) != 1 {
		t.Errorf("with a charset parameter: status = %d, calls = %v; want 200 and one call", w.Code, h.calls)
	}
}

// TestInteractionsHandler_ServeHTTP_OversizedBody_Rejected checks a body over 64 KB is refused before the handler runs.
func TestInteractionsHandler_ServeHTTP_OversizedBody_Rejected(t *testing.T) {
	h := &recordingHandler{}
	big := strings.Replace(shortcutOnRootEnvelope, "Has anyone", strings.Repeat("a", 64<<10), 1)
	if w := serveInteraction(h, interactionRequest(big)); w.Code != http.StatusRequestEntityTooLarge || len(h.calls) != 0 {
		t.Errorf("status = %d, calls = %v; want 413 and no call", w.Code, h.calls)
	}
}

// TestInteractionsHandler_ServeHTTP_MalformedBody_BadRequest checks a body that is not one JSON object is refused.
func TestInteractionsHandler_ServeHTTP_MalformedBody_BadRequest(t *testing.T) {
	for _, body := range []string{"", "{", `["message_shortcut"]`, shortcutOnRootEnvelope + "{}"} {
		h := &recordingHandler{}
		if w := serveInteraction(h, interactionRequest(body)); w.Code != http.StatusBadRequest || len(h.calls) != 0 {
			t.Errorf("body %.20q: status = %d, calls = %v; want 400 and no call", body, w.Code, h.calls)
		}
	}
}

// TestInteractionsHandler_ServeHTTP_NotPost_MethodNotAllowed checks an authenticated request that is not a POST is refused.
func TestInteractionsHandler_ServeHTTP_NotPost_MethodNotAllowed(t *testing.T) {
	h := &recordingHandler{}
	r := interactionRequest(shortcutOnRootEnvelope)
	r.Method = http.MethodPut
	if w := serveInteraction(h, r); w.Code != http.StatusMethodNotAllowed || len(h.calls) != 0 {
		t.Errorf("status = %d, calls = %v; want 405 and no call", w.Code, h.calls)
	}
}

// TestInteractionsHandler_ServeHTTP_ShortcutOnRoot_ExactEvent checks a shortcut on a thread's first message carries no thread ts.
func TestInteractionsHandler_ServeHTTP_ShortcutOnRoot_ExactEvent(t *testing.T) {
	want := ShortcutEvent{
		TriggerID:     "9001.43.abcdef",
		TeamID:        "T0FORGE",
		UserID:        "UMOVER",
		ChannelID:     "C1",
		MessageTS:     "1760950800.000100",
		MessageText:   "Has anyone moved from Ingress to *Gateway API*?",
		MessageUserID: "UASKER",
	}
	// Slack reports a root that has replies as its own thread; the bot may
	// pass that through or leave thread_ts empty.
	selfThreaded := strings.Replace(shortcutOnRootEnvelope, `"thread_ts": ""`, `"thread_ts": "1760950800.000100"`, 1)
	for name, envelope := range map[string]string{"no thread_ts": shortcutOnRootEnvelope, "thread_ts names itself": selfThreaded} {
		t.Run(name, func(t *testing.T) {
			h := &recordingHandler{}
			w := serveInteraction(h, interactionRequest(envelope))
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", w.Code)
			}
			if !reflect.DeepEqual(h.calls, []string{"shortcut"}) || h.shortcut != want {
				t.Errorf("calls = %v, event = %+v\nwant one shortcut %+v", h.calls, h.shortcut, want)
			}
		})
	}
}

// TestInteractionsHandler_ServeHTTP_ShortcutOnReply_ExactEvent checks a shortcut on a reply carries the reply and its thread root.
func TestInteractionsHandler_ServeHTTP_ShortcutOnReply_ExactEvent(t *testing.T) {
	h := &recordingHandler{}
	w := serveInteraction(h, interactionRequest(shortcutOnReplyEnvelope))
	want := ShortcutEvent{
		TriggerID:     "9001.42.abcdef",
		TeamID:        "T0FORGE",
		UserID:        "UMOVER",
		ChannelID:     "C1",
		MessageTS:     "1760950860.000200",
		ThreadTS:      "1760950800.000100",
		MessageText:   "Yes, <@UASKER> we did it last quarter.",
		MessageUserID: "UR1",
	}
	if w.Code != http.StatusOK || !reflect.DeepEqual(h.calls, []string{"shortcut"}) || h.shortcut != want {
		t.Errorf("status = %d, calls = %v, event = %+v\nwant 200 and one shortcut %+v", w.Code, h.calls, h.shortcut, want)
	}
}

// TestInteractionsHandler_ServeHTTP_ShortcutOnBotMessage_CarriesBotID checks the message's bot ID reaches the handler.
func TestInteractionsHandler_ServeHTTP_ShortcutOnBotMessage_CarriesBotID(t *testing.T) {
	h := &recordingHandler{}
	envelope := strings.Replace(shortcutOnRootEnvelope, `"user": "UASKER", "bot_id": ""`, `"user": "", "bot_id": "B0DEPLOY"`, 1)
	serveInteraction(h, interactionRequest(envelope))
	if h.shortcut.MessageBotID != "B0DEPLOY" || h.shortcut.MessageUserID != "" {
		t.Errorf("event = %+v, want bot ID B0DEPLOY and no user", h.shortcut)
	}
}

// ctxCheckingHandler records whether the shortcut context was live during the call.
type ctxCheckingHandler struct {
	recordingHandler
	errDuringCall error
	hasDeadline   bool
}

func (h *ctxCheckingHandler) HandleShortcut(ctx context.Context, sc ShortcutEvent) error {
	h.errDuringCall = ctx.Err()
	_, h.hasDeadline = ctx.Deadline()
	return h.recordingHandler.HandleShortcut(ctx, sc)
}

// TestInteractionsHandler_ServeHTTP_ShortcutRequestCancelled_DialogStillOpens checks a cancelled request does not cancel the shortcut's context, which still has a deadline.
func TestInteractionsHandler_ServeHTTP_ShortcutRequestCancelled_DialogStillOpens(t *testing.T) {
	h := &ctxCheckingHandler{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := serveInteraction(h, interactionRequest(shortcutOnRootEnvelope).WithContext(ctx))
	if w.Code != http.StatusOK || len(h.calls) != 1 {
		t.Fatalf("status = %d, calls = %v; want 200 and one call", w.Code, h.calls)
	}
	if h.errDuringCall != nil {
		t.Errorf("handler context during the call: %v, want live", h.errDuringCall)
	}
	if !h.hasDeadline {
		t.Error("handler context has no deadline")
	}
}

// TestInteractionsHandler_ServeHTTP_ShortcutHandlerError_ServerError checks a failed shortcut answers 500 so the bot tells the member.
func TestInteractionsHandler_ServeHTTP_ShortcutHandlerError_ServerError(t *testing.T) {
	h := &recordingHandler{shortcutErr: errors.New("slack is down: secret detail")}
	w := serveInteraction(h, interactionRequest(shortcutOnRootEnvelope))
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
	if strings.Contains(w.Body.String(), "secret detail") {
		t.Errorf("body leaks the error: %q", w.Body.String())
	}
}

// TestInteractionsHandler_ServeHTTP_Submission_ExactEvent checks the title, both tags and the metadata are read from the envelope, and no errors answers {}.
func TestInteractionsHandler_ServeHTTP_Submission_ExactEvent(t *testing.T) {
	h := &recordingHandler{}
	w := serveInteraction(h, interactionRequest(submissionEnvelope))
	want := SubmissionEvent{
		TeamID:          "T0FORGE",
		UserID:          "UMOVER",
		UserName:        "mo",
		Title:           "Rolling out Gateway API",
		Tags:            []string{"kubernetes", "architecture"},
		PrivateMetadata: `{"channel":"C1","ts":"1760950800.000100"}`,
	}
	if !reflect.DeepEqual(h.calls, []string{"submission"}) || !reflect.DeepEqual(h.submission, want) {
		t.Errorf("calls = %v, event = %+v\nwant one submission %+v", h.calls, h.submission, want)
	}
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != "{}" {
		t.Errorf("status = %d, body = %q; want 200 and {}", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

// TestInteractionsHandler_ServeHTTP_SubmissionFieldErrors_ReturnedAsErrors checks field errors come back keyed by block ID.
func TestInteractionsHandler_ServeHTTP_SubmissionFieldErrors_ReturnedAsErrors(t *testing.T) {
	h := &recordingHandler{fieldErrors: map[string]string{MoveTagsBlockID: "Pick at least one tag."}}
	w := serveInteraction(h, interactionRequest(submissionEnvelope))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got, want := strings.TrimSpace(w.Body.String()), `{"errors":{"tags":"Pick at least one tag."}}`; got != want {
		t.Errorf("body = %s\nwant %s", got, want)
	}
}

// TestInteractionsHandler_ServeHTTP_SubmissionNothingEntered_EmptyEvent checks a null title and no selected tags decode to an empty submission.
func TestInteractionsHandler_ServeHTTP_SubmissionNothingEntered_EmptyEvent(t *testing.T) {
	h := &recordingHandler{}
	w := serveInteraction(h, interactionRequest(`{"type": "view_submission", "callback_id": "move_to_forum_submit",
		"team_id": "T0FORGE", "user": {"id": "UMOVER", "name": "mo"},
		"view": {"id": "V1", "callback_id": "move_to_forum_submit", "private_metadata": "", "values": {
			"title": {"title_input": {"type": "plain_text_input", "value": null}},
			"tags": {"tags_select": {"type": "multi_static_select", "selected_options": []}}}}}`))
	if w.Code != http.StatusOK || len(h.calls) != 1 || h.submission.Title != "" || len(h.submission.Tags) != 0 {
		t.Errorf("status = %d, calls = %v, event = %+v; want 200 and an empty submission", w.Code, h.calls, h.submission)
	}
}

// TestInteractionsHandler_ServeHTTP_OtherTeam_Ignored checks an interaction from another workspace answers 200 without a handler call.
func TestInteractionsHandler_ServeHTTP_OtherTeam_Ignored(t *testing.T) {
	for name, envelope := range map[string]string{"shortcut": shortcutOnRootEnvelope, "submission": submissionEnvelope} {
		h := &recordingHandler{}
		w := serveInteraction(h, interactionRequest(strings.Replace(envelope, `"team_id": "T0FORGE"`, `"team_id": "T0OTHER"`, 1)))
		if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != "{}" || len(h.calls) != 0 {
			t.Errorf("%s: status = %d, body = %q, calls = %v; want 200, {} and no call", name, w.Code, w.Body.String(), h.calls)
		}
	}
}

// TestInteractionsHandler_ServeHTTP_UnknownInteraction_Ignored checks other types and callback IDs answer 200 {} without a handler call.
func TestInteractionsHandler_ServeHTTP_UnknownInteraction_Ignored(t *testing.T) {
	envelopes := map[string]string{
		"another message shortcut": strings.Replace(shortcutOnRootEnvelope, `"callback_id": "move_to_forum"`, `"callback_id": "something_else"`, 1),
		"another dialog":           strings.ReplaceAll(submissionEnvelope, `"callback_id": "move_to_forum_submit"`, `"callback_id": "something_else"`),
		"a global shortcut":        `{"type": "global_shortcut", "callback_id": "move_to_forum", "team_id": "T0FORGE", "user": {"id": "UMOVER", "name": "mo"}}`,
		"shortcut ID on a dialog":  strings.Replace(submissionEnvelope, `"callback_id": "move_to_forum_submit"`, `"callback_id": "move_to_forum"`, 1),
		"dialog ID on a shortcut":  strings.Replace(shortcutOnRootEnvelope, `"callback_id": "move_to_forum"`, `"callback_id": "move_to_forum_submit"`, 1),
		"a button click":           `{"type": "block_actions", "callback_id": "move_to_forum", "team_id": "T0FORGE"}`,
	}
	for name, envelope := range envelopes {
		t.Run(name, func(t *testing.T) {
			h := &recordingHandler{}
			w := serveInteraction(h, interactionRequest(envelope))
			if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != "{}" || len(h.calls) != 0 {
				t.Errorf("status = %d, body = %q, calls = %v; want 200, {} and no call", w.Code, w.Body.String(), h.calls)
			}
		})
	}
}
