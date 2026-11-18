package slackx

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/slack-go/slack"
)

// Payloads as Slack sends them in a Socket Mode "interactive" envelope.
const (
	shortcutOnReplyJSON = `{
		"type": "message_action",
		"token": "legacy-token",
		"action_ts": "1760951000.123456",
		"team": {"id": "T0FORGE", "domain": "forgeutah"},
		"user": {"id": "UMOVER", "username": "mo", "team_id": "T0FORGE", "name": "mo"},
		"channel": {"id": "C1", "name": "kubernetes"},
		"is_enterprise_install": false,
		"enterprise": null,
		"callback_id": "move_to_forum",
		"trigger_id": "9001.42.abcdef",
		"response_url": "https://hooks.slack.test/app/T0FORGE/1/x",
		"message_ts": "1760950860.000200",
		"message": {
			"client_msg_id": "6a1f4e0c-0000-4000-8000-000000000001",
			"type": "message",
			"text": "Yes, <@UASKER> we did it last quarter.",
			"user": "UR1",
			"ts": "1760950860.000200",
			"team": "T0FORGE",
			"thread_ts": "1760950800.000100",
			"parent_user_id": "UASKER",
			"blocks": [{"type": "rich_text", "block_id": "b1", "elements": [
				{"type": "rich_text_section", "elements": [{"type": "text", "text": "Yes, we did it last quarter."}]}]}]
		}
	}`

	shortcutOnRootJSON = `{
		"type": "message_action",
		"team": {"id": "T0FORGE", "domain": "forgeutah"},
		"user": {"id": "UMOVER", "username": "mo", "team_id": "T0FORGE", "name": "mo"},
		"channel": {"id": "C1", "name": "kubernetes"},
		"callback_id": "move_to_forum",
		"trigger_id": "9001.43.abcdef",
		"message_ts": "1760950800.000100",
		"message": {
			"type": "message",
			"text": "Has anyone moved from Ingress to *Gateway API*?",
			"user": "UASKER",
			"ts": "1760950800.000100",
			"team": "T0FORGE",
			"thread_ts": "1760950800.000100",
			"reply_count": 50,
			"latest_reply": "1760959999.000900"
		}
	}`

	submissionJSON = `{
		"type": "view_submission",
		"team": {"id": "T0FORGE", "domain": "forgeutah"},
		"user": {"id": "UMOVER", "username": "mo", "name": "mo", "team_id": "T0FORGE"},
		"api_app_id": "A0FORUM",
		"token": "legacy-token",
		"trigger_id": "9002.44.abcdef",
		"view": {
			"id": "V1",
			"team_id": "T0FORGE",
			"type": "modal",
			"private_metadata": "{\"channel\":\"C1\",\"ts\":\"1760950800.000100\"}",
			"callback_id": "move_to_forum_submit",
			"state": {"values": {
				"title": {"title_input": {"type": "plain_text_input", "value": "Rolling out Gateway API"}},
				"tags": {"tags_select": {"type": "multi_static_select", "selected_options": [
					{"text": {"type": "plain_text", "text": "kubernetes", "emoji": true}, "value": "kubernetes"},
					{"text": {"type": "plain_text", "text": "architecture", "emoji": true}, "value": "architecture"}]}}
			}},
			"hash": "1760951000.abc",
			"title": {"type": "plain_text", "text": "Move to forum"},
			"submit": {"type": "plain_text", "text": "Move thread"},
			"close": {"type": "plain_text", "text": "Cancel"},
			"root_view_id": "V1",
			"app_id": "A0FORUM",
			"bot_id": "B0FORUM"
		},
		"response_urls": [],
		"is_enterprise_install": false,
		"enterprise": null
	}`
)

// parseInteraction decodes a payload the way slack-go's Socket Mode client does.
func parseInteraction(t *testing.T, payload string) slack.InteractionCallback {
	t.Helper()
	var cb slack.InteractionCallback
	if err := json.Unmarshal([]byte(payload), &cb); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	return cb
}

// TestDecodeInteraction_ShortcutOnReply checks a shortcut on a reply carries both the reply and its thread root.
func TestDecodeInteraction_ShortcutOnReply(t *testing.T) {
	kind, sc, _ := decodeInteraction(parseInteraction(t, shortcutOnReplyJSON))
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
	if kind != interactionShortcut || sc != want {
		t.Errorf("decodeInteraction = %v, %+v\nwant shortcut %+v", kind, sc, want)
	}
}

// TestDecodeInteraction_ShortcutOnRoot checks a thread root is not reported as a reply to itself.
func TestDecodeInteraction_ShortcutOnRoot(t *testing.T) {
	kind, sc, _ := decodeInteraction(parseInteraction(t, shortcutOnRootJSON))
	if kind != interactionShortcut {
		t.Fatalf("kind = %v, want a shortcut", kind)
	}
	if sc.MessageTS != "1760950800.000100" || sc.ThreadTS != "" {
		t.Errorf("MessageTS = %q, ThreadTS = %q; want the root ts and no thread ts", sc.MessageTS, sc.ThreadTS)
	}
	if sc.MessageText != "Has anyone moved from Ingress to *Gateway API*?" || sc.MessageUserID != "UASKER" {
		t.Errorf("message = %q by %q", sc.MessageText, sc.MessageUserID)
	}
}

// TestDecodeInteraction_Submission checks the title and tags are read through the block and action IDs.
func TestDecodeInteraction_Submission(t *testing.T) {
	kind, _, sub := decodeInteraction(parseInteraction(t, submissionJSON))
	want := SubmissionEvent{
		TeamID:          "T0FORGE",
		UserID:          "UMOVER",
		UserName:        "mo",
		Title:           "Rolling out Gateway API",
		Tags:            []string{"kubernetes", "architecture"},
		PrivateMetadata: `{"channel":"C1","ts":"1760950800.000100"}`,
	}
	if kind != interactionSubmission || !reflect.DeepEqual(sub, want) {
		t.Errorf("decodeInteraction = %v, %+v\nwant submission %+v", kind, sub, want)
	}
}

// TestDecodeInteraction_SubmissionWithNothingSelected checks an empty dialog decodes to an empty title and no tags.
func TestDecodeInteraction_SubmissionWithNothingSelected(t *testing.T) {
	cb := parseInteraction(t, `{"type": "view_submission", "team": {"id": "T0FORGE"}, "user": {"id": "UMOVER"},
		"view": {"callback_id": "move_to_forum_submit", "state": {"values": {
			"title": {"title_input": {"type": "plain_text_input", "value": null}},
			"tags": {"tags_select": {"type": "multi_static_select", "selected_options": []}}}}}}`)
	kind, _, sub := decodeInteraction(cb)
	if kind != interactionSubmission || sub.Title != "" || len(sub.Tags) != 0 {
		t.Errorf("decodeInteraction = %v, %+v; want an empty submission", kind, sub)
	}
}

// TestDecodeInteraction_OtherCallbacksIgnored checks interactions that are not "Move to forum" are left alone.
func TestDecodeInteraction_OtherCallbacksIgnored(t *testing.T) {
	payloads := map[string]string{
		"another message shortcut": `{"type": "message_action", "callback_id": "something_else", "message": {"ts": "1.0"}}`,
		"another modal":            `{"type": "view_submission", "view": {"callback_id": "something_else"}}`,
		"a closed modal":           `{"type": "view_closed", "view": {"callback_id": "move_to_forum_submit"}}`,
		"a button click":           `{"type": "block_actions", "callback_id": "move_to_forum"}`,
		"a global shortcut":        `{"type": "shortcut", "callback_id": "move_to_forum"}`,
	}
	for name, payload := range payloads {
		t.Run(name, func(t *testing.T) {
			if kind, _, _ := decodeInteraction(parseInteraction(t, payload)); kind != interactionIgnored {
				t.Errorf("kind = %v, want ignored", kind)
			}
		})
	}
}

// recordingHandler records the order of acknowledgements and handler calls.
type recordingHandler struct {
	steps       []string
	shortcut    ShortcutEvent
	submission  SubmissionEvent
	shortcutErr error
	fieldErrors map[string]string
}

func (h *recordingHandler) HandleShortcut(_ context.Context, sc ShortcutEvent) error {
	h.steps = append(h.steps, "shortcut")
	h.shortcut = sc
	return h.shortcutErr
}

func (h *recordingHandler) HandleSubmission(_ context.Context, sub SubmissionEvent) map[string]string {
	h.steps = append(h.steps, "submission")
	h.submission = sub
	return h.fieldErrors
}

// dispatch runs dispatchInteraction and returns the payloads it acknowledged with.
func dispatch(t *testing.T, h *recordingHandler, payload string) []any {
	t.Helper()
	var acks []any
	ack := func(p any) {
		h.steps = append(h.steps, "ack")
		acks = append(acks, p)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	dispatchInteraction(context.Background(), parseInteraction(t, payload), h, ack, logger)
	return acks
}

// TestDispatchInteraction_ShortcutAckedBeforeHandling checks the shortcut is acknowledged before any other work.
func TestDispatchInteraction_ShortcutAckedBeforeHandling(t *testing.T) {
	h := &recordingHandler{shortcutErr: errors.New("slack is down")}
	acks := dispatch(t, h, shortcutOnReplyJSON)
	if !reflect.DeepEqual(h.steps, []string{"ack", "shortcut"}) {
		t.Errorf("steps = %v, want ack then shortcut", h.steps)
	}
	if len(acks) != 1 || acks[0] != nil {
		t.Errorf("acks = %v, want one empty ack", acks)
	}
	if h.shortcut.ThreadTS != "1760950800.000100" {
		t.Errorf("handler got %+v", h.shortcut)
	}
}

// TestDispatchInteraction_ValidSubmissionClosesDialog checks a valid submission gets an empty ack.
func TestDispatchInteraction_ValidSubmissionClosesDialog(t *testing.T) {
	h := &recordingHandler{}
	acks := dispatch(t, h, submissionJSON)
	if !reflect.DeepEqual(h.steps, []string{"submission", "ack"}) {
		t.Errorf("steps = %v, want submission then ack", h.steps)
	}
	if len(acks) != 1 || acks[0] != nil {
		t.Errorf("acks = %v, want one empty ack", acks)
	}
	if h.submission.Title != "Rolling out Gateway API" {
		t.Errorf("handler got %+v", h.submission)
	}
}

// TestDispatchInteraction_InvalidSubmissionAcksFieldErrors checks field errors reach Slack as response_action "errors".
func TestDispatchInteraction_InvalidSubmissionAcksFieldErrors(t *testing.T) {
	h := &recordingHandler{fieldErrors: map[string]string{MoveTagsBlockID: "Pick at least one tag."}}
	acks := dispatch(t, h, submissionJSON)
	if len(acks) != 1 {
		t.Fatalf("acks = %v, want one", acks)
	}
	got, err := json.Marshal(acks[0])
	if err != nil {
		t.Fatalf("encode ack: %v", err)
	}
	if want := `{"response_action":"errors","errors":{"tags":"Pick at least one tag."}}`; string(got) != want {
		t.Errorf("ack payload = %s\nwant %s", got, want)
	}
}

// TestDispatchInteraction_OtherInteractionAckedAndIgnored checks an unrelated interaction is acknowledged without a handler call.
func TestDispatchInteraction_OtherInteractionAckedAndIgnored(t *testing.T) {
	h := &recordingHandler{}
	acks := dispatch(t, h, `{"type": "block_actions", "callback_id": "something_else"}`)
	if !reflect.DeepEqual(h.steps, []string{"ack"}) || len(acks) != 1 || acks[0] != nil {
		t.Errorf("steps = %v, acks = %v; want a single empty ack", h.steps, acks)
	}
}

// socketStub is a Slack Web API stub that answers apps.connections.open with body.
func socketStub(t *testing.T, body string) (*RealClient, <-chan string) {
	t.Helper()
	auth := make(chan string, 16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/apps.connections.open" {
			select {
			case auth <- r.Header.Get("Authorization"):
			default:
			}
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return NewClient("xoxb-bot", srv.URL, srv.Client()), auth
}

// TestRunSocketMode_UsesAppTokenAndStopsOnCancel checks the connection opens with the app-level token and ends with ctx.
func TestRunSocketMode_UsesAppTokenAndStopsOnCancel(t *testing.T) {
	client, auth := socketStub(t, `{"ok": false, "error": "internal_error"}`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- RunSocketMode(ctx, client, "xapp-app", &recordingHandler{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()

	if got := <-auth; got != "Bearer xapp-app" {
		t.Errorf("apps.connections.open Authorization = %q, want the app-level token", got)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("RunSocketMode after cancel = %v, want nil", err)
	}
}

// TestRunSocketMode_RejectedTokenReturnsError checks a token Slack rejects ends the loop with an error.
func TestRunSocketMode_RejectedTokenReturnsError(t *testing.T) {
	client, _ := socketStub(t, `{"ok": false, "error": "invalid_auth"}`)
	err := RunSocketMode(context.Background(), client, "xapp-bad", &recordingHandler{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		t.Fatal("RunSocketMode with a rejected token returned nil")
	}
}
