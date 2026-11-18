package slackx

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sync"
	"testing"
	"time"
)

// apiCall is one request the stub Slack server received.
type apiCall struct {
	Method string     // Web API method name, e.g. "chat.postMessage"
	Form   url.Values // form body merged with the query string
	Body   []byte     // raw body, for JSON methods
}

// stubSlack is an httptest server speaking Slack's Web API JSON.
type stubSlack struct {
	t     *testing.T
	mu    sync.Mutex
	calls []apiCall
	// respond returns the HTTP status and JSON body for a call. n counts
	// calls to that method, starting at 1.
	respond func(c apiCall, n int) (int, http.Header, string)
}

// newStubSlack starts a stub server and returns it with a client aimed
// at it.
func newStubSlack(t *testing.T, respond func(c apiCall, n int) (int, http.Header, string)) (*stubSlack, *RealClient) {
	t.Helper()
	s := &stubSlack{t: t, respond: respond}
	srv := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(srv.Close)
	return s, NewClient("xoxb-test", srv.URL, srv.Client())
}

func (s *stubSlack) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	c := apiCall{Method: r.URL.Path[1:], Form: r.URL.Query(), Body: body}
	if r.Header.Get("Content-Type") == "application/x-www-form-urlencoded" {
		if form, err := url.ParseQuery(string(body)); err == nil {
			for k, v := range form {
				c.Form[k] = append(c.Form[k], v...)
			}
		}
	}
	s.mu.Lock()
	s.calls = append(s.calls, c)
	n := 0
	for _, prev := range s.calls {
		if prev.Method == c.Method {
			n++
		}
	}
	s.mu.Unlock()

	status, header, resp := s.respond(c, n)
	for k, v := range header {
		w.Header()[k] = v
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	io.WriteString(w, resp)
}

// methods returns the method names called, in order.
func (s *stubSlack) methods() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, c := range s.calls {
		out = append(out, c.Method)
	}
	return out
}

// only returns the single call made to method, failing otherwise.
func (s *stubSlack) only(method string) apiCall {
	s.t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var found []apiCall
	for _, c := range s.calls {
		if c.Method == method {
			found = append(found, c)
		}
	}
	if len(found) != 1 {
		s.t.Fatalf("calls to %s = %d, want 1", method, len(found))
	}
	return found[0]
}

func ok(body string) (int, http.Header, string) { return http.StatusOK, nil, body }

// TestRealClient_PostMessage_DisablesUnfurlsAndThreads checks the form
// body carries the text, both unfurl flags off, and the thread ts.
func TestRealClient_PostMessage_DisablesUnfurlsAndThreads(t *testing.T) {
	s, c := newStubSlack(t, func(apiCall, int) (int, http.Header, string) {
		return ok(`{"ok":true,"channel":"C1","ts":"2.0"}`)
	})

	if err := c.PostMessage(context.Background(), "C1", "<@U1> moved this thread to the forum.", "1.0"); err != nil {
		t.Fatalf("PostMessage: %v", err)
	}

	form := s.only("chat.postMessage").Form
	for key, want := range map[string]string{
		"channel":      "C1",
		"text":         "<@U1> moved this thread to the forum.",
		"thread_ts":    "1.0",
		"unfurl_links": "false",
		"unfurl_media": "false",
	} {
		if got := form.Get(key); got != want {
			t.Errorf("form %s = %q, want %q", key, got, want)
		}
	}
}

// TestRealClient_PostMessage_NoThread_OmitsThreadTS checks a top-level
// post or direct message sends no thread_ts.
func TestRealClient_PostMessage_NoThread_OmitsThreadTS(t *testing.T) {
	s, c := newStubSlack(t, func(apiCall, int) (int, http.Header, string) {
		return ok(`{"ok":true,"channel":"D1","ts":"2.0"}`)
	})

	if err := c.PostMessage(context.Background(), "U7", "hello", ""); err != nil {
		t.Fatalf("PostMessage: %v", err)
	}

	form := s.only("chat.postMessage").Form
	if form.Get("channel") != "U7" {
		t.Errorf("channel = %q, want the user ID", form.Get("channel"))
	}
	if form.Has("thread_ts") {
		t.Errorf("thread_ts = %q, want it absent", form.Get("thread_ts"))
	}
}

// TestRealClient_PostMessage_SlackError_ReturnsAPIError checks an
// ok:false reply surfaces its code through ErrorCode.
func TestRealClient_PostMessage_SlackError_ReturnsAPIError(t *testing.T) {
	_, c := newStubSlack(t, func(apiCall, int) (int, http.Header, string) {
		return ok(`{"ok":false,"error":"channel_not_found"}`)
	})

	err := c.PostMessage(context.Background(), "C404", "hello", "")
	if got := ErrorCode(err); got != "channel_not_found" {
		t.Fatalf("ErrorCode(%v) = %q, want channel_not_found", err, got)
	}
}

// TestRealClient_PostMessage_RateLimited_ReturnsRetryAfter checks a 429
// becomes a RateLimitedError carrying Slack's Retry-After.
func TestRealClient_PostMessage_RateLimited_ReturnsRetryAfter(t *testing.T) {
	_, c := newStubSlack(t, func(apiCall, int) (int, http.Header, string) {
		return http.StatusTooManyRequests, http.Header{"Retry-After": {"17"}}, `{"ok":false,"error":"ratelimited"}`
	})

	err := c.PostMessage(context.Background(), "C1", "hello", "")
	var limited *RateLimitedError
	if !errors.As(err, &limited) {
		t.Fatalf("error = %v (%T), want *RateLimitedError", err, err)
	}
	if limited.RetryAfter != 17*time.Second {
		t.Errorf("RetryAfter = %s, want 17s", limited.RetryAfter)
	}
}

// TestRealClient_ThreadReplies_TwoPages_FollowsCursor checks pagination
// is followed and messages come back root first with their fields.
func TestRealClient_ThreadReplies_TwoPages_FollowsCursor(t *testing.T) {
	s, c := newStubSlack(t, func(call apiCall, _ int) (int, http.Header, string) {
		if call.Form.Get("cursor") == "" {
			return ok(`{"ok":true,"has_more":true,"response_metadata":{"next_cursor":"page2"},"messages":[
				{"type":"message","user":"U1","text":"root","ts":"1.0","thread_ts":"1.0"},
				{"type":"message","user":"U2","text":"first","ts":"2.0","thread_ts":"1.0"}]}`)
		}
		return ok(`{"ok":true,"has_more":false,"messages":[
			{"type":"message","subtype":"bot_message","bot_id":"B9","text":"second","ts":"3.0","thread_ts":"1.0"}]}`)
	})

	got, err := c.ThreadReplies(context.Background(), "C1", "1.0")
	if err != nil {
		t.Fatalf("ThreadReplies: %v", err)
	}

	want := []Message{
		{User: "U1", Text: "root", TS: "1.0", ThreadTS: "1.0"},
		{User: "U2", Text: "first", TS: "2.0", ThreadTS: "1.0"},
		{BotID: "B9", Text: "second", TS: "3.0", ThreadTS: "1.0", SubType: "bot_message"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("messages = %+v\nwant %+v", got, want)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(s.calls))
	}
	if got := s.calls[0].Form.Get("limit"); got != "200" {
		t.Errorf("limit = %q, want 200", got)
	}
	if got := s.calls[1].Form.Get("cursor"); got != "page2" {
		t.Errorf("second call cursor = %q, want page2", got)
	}
}

// TestRealClient_ThreadReplies_ReplyTS_ResolvesToRoot checks that asking
// about a reply returns the whole thread it belongs to.
func TestRealClient_ThreadReplies_ReplyTS_ResolvesToRoot(t *testing.T) {
	_, c := newStubSlack(t, func(call apiCall, _ int) (int, http.Header, string) {
		if call.Form.Get("ts") == "2.0" {
			return ok(`{"ok":true,"messages":[{"type":"message","user":"U2","text":"first","ts":"2.0","thread_ts":"1.0"}]}`)
		}
		return ok(`{"ok":true,"messages":[
			{"type":"message","user":"U1","text":"root","ts":"1.0","thread_ts":"1.0"},
			{"type":"message","user":"U2","text":"first","ts":"2.0","thread_ts":"1.0"}]}`)
	})

	got, err := c.ThreadReplies(context.Background(), "C1", "2.0")
	if err != nil {
		t.Fatalf("ThreadReplies: %v", err)
	}
	if len(got) != 2 || got[0].TS != "1.0" || got[1].TS != "2.0" {
		t.Errorf("messages = %+v, want the root then the reply", got)
	}
}

// TestRealClient_ThreadReplies_NotInChannel_JoinsOnceAndRetries checks
// the join-and-retry path for a public channel the bot is not in.
func TestRealClient_ThreadReplies_NotInChannel_JoinsOnceAndRetries(t *testing.T) {
	s, c := newStubSlack(t, func(call apiCall, n int) (int, http.Header, string) {
		switch {
		case call.Method == "conversations.join":
			return ok(`{"ok":true,"channel":{"id":"C1","name":"general"}}`)
		case n == 1:
			return ok(`{"ok":false,"error":"not_in_channel"}`)
		default:
			return ok(`{"ok":true,"messages":[{"type":"message","user":"U1","text":"root","ts":"1.0"}]}`)
		}
	})

	got, err := c.ThreadReplies(context.Background(), "C1", "1.0")
	if err != nil {
		t.Fatalf("ThreadReplies: %v", err)
	}
	if len(got) != 1 || got[0].Text != "root" {
		t.Errorf("messages = %+v, want the root", got)
	}
	want := []string{"conversations.replies", "conversations.join", "conversations.replies"}
	if methods := s.methods(); !reflect.DeepEqual(methods, want) {
		t.Errorf("calls = %v, want %v", methods, want)
	}
	if ch := s.only("conversations.join").Form.Get("channel"); ch != "C1" {
		t.Errorf("joined %q, want C1", ch)
	}
}

// TestRealClient_ThreadReplies_StillNotInChannel_DoesNotLoop checks the
// join is tried only once.
func TestRealClient_ThreadReplies_StillNotInChannel_DoesNotLoop(t *testing.T) {
	s, c := newStubSlack(t, func(call apiCall, _ int) (int, http.Header, string) {
		if call.Method == "conversations.join" {
			return ok(`{"ok":true,"channel":{"id":"C1"}}`)
		}
		return ok(`{"ok":false,"error":"not_in_channel"}`)
	})

	_, err := c.ThreadReplies(context.Background(), "C1", "1.0")
	if ErrorCode(err) != "not_in_channel" {
		t.Fatalf("error = %v, want not_in_channel", err)
	}
	if n := len(s.methods()); n != 3 {
		t.Errorf("calls = %d, want 3 (replies, join, replies)", n)
	}
}

// TestRealClient_UserInfo_NameFallback checks display name wins, then
// real name, and the 72px avatar and bot flag carry over.
func TestRealClient_UserInfo_NameFallback(t *testing.T) {
	_, c := newStubSlack(t, func(call apiCall, _ int) (int, http.Header, string) {
		switch call.Form.Get("user") {
		case "U1":
			return ok(`{"ok":true,"user":{"id":"U1","name":"dana","real_name":"Dana Reyes","is_bot":false,
				"profile":{"display_name":"dana.r","real_name":"Dana Reyes","image_72":"https://a.test/72.png","image_192":"https://a.test/192.png"}}}`)
		case "U2":
			return ok(`{"ok":true,"user":{"id":"U2","name":"sam","real_name":"Sam Okafor","is_bot":true,
				"profile":{"display_name":"","real_name":"Sam Okafor","image_72":""}}}`)
		}
		return ok(`{"ok":false,"error":"user_not_found"}`)
	})
	ctx := context.Background()

	u1, err := c.UserInfo(ctx, "U1")
	if err != nil {
		t.Fatalf("UserInfo U1: %v", err)
	}
	if want := (User{ID: "U1", Name: "dana.r", AvatarURL: "https://a.test/72.png"}); u1 != want {
		t.Errorf("U1 = %+v, want %+v", u1, want)
	}

	u2, err := c.UserInfo(ctx, "U2")
	if err != nil {
		t.Fatalf("UserInfo U2: %v", err)
	}
	if want := (User{ID: "U2", Name: "Sam Okafor", IsBot: true}); u2 != want {
		t.Errorf("U2 = %+v, want %+v", u2, want)
	}

	if _, err := c.UserInfo(ctx, "U404"); ErrorCode(err) != "user_not_found" {
		t.Errorf("U404 error = %v, want user_not_found", err)
	}
}

// TestRealClient_ChannelInfo_PublicVersusOthers checks only a plain
// public channel reports IsPublic.
func TestRealClient_ChannelInfo_PublicVersusOthers(t *testing.T) {
	_, c := newStubSlack(t, func(call apiCall, _ int) (int, http.Header, string) {
		switch call.Form.Get("channel") {
		case "C1":
			return ok(`{"ok":true,"channel":{"id":"C1","name":"ai-driven-development","is_channel":true,"is_private":false}}`)
		case "G1":
			return ok(`{"ok":true,"channel":{"id":"G1","name":"board","is_channel":true,"is_group":false,"is_private":true}}`)
		case "D1":
			return ok(`{"ok":true,"channel":{"id":"D1","is_im":true,"is_private":true,"user":"U1"}}`)
		case "M1":
			return ok(`{"ok":true,"channel":{"id":"M1","name":"mpdm-a--b--c-1","is_mpim":true,"is_group":true,"is_private":true}}`)
		}
		return ok(`{"ok":false,"error":"channel_not_found"}`)
	})
	ctx := context.Background()

	for id, want := range map[string]Channel{
		"C1": {ID: "C1", Name: "ai-driven-development", IsPublic: true},
		"G1": {ID: "G1", Name: "board"},
		"D1": {ID: "D1"},
		"M1": {ID: "M1", Name: "mpdm-a--b--c-1"},
	} {
		got, err := c.ChannelInfo(ctx, id)
		if err != nil {
			t.Errorf("ChannelInfo %s: %v", id, err)
			continue
		}
		if got != want {
			t.Errorf("ChannelInfo %s = %+v, want %+v", id, got, want)
		}
	}
}

// TestRealClient_Permalink_ReturnsLink checks chat.getPermalink is asked
// for the right message.
func TestRealClient_Permalink_ReturnsLink(t *testing.T) {
	s, c := newStubSlack(t, func(apiCall, int) (int, http.Header, string) {
		return ok(`{"ok":true,"channel":"C1","permalink":"https://forge.slack.test/archives/C1/p10"}`)
	})

	link, err := c.Permalink(context.Background(), "C1", "1.0")
	if err != nil {
		t.Fatalf("Permalink: %v", err)
	}
	if link != "https://forge.slack.test/archives/C1/p10" {
		t.Errorf("link = %q", link)
	}
	form := s.only("chat.getPermalink").Form
	if form.Get("channel") != "C1" || form.Get("message_ts") != "1.0" {
		t.Errorf("query = %v, want channel C1 and message_ts 1.0", form)
	}
}

// TestRealClient_OpenMoveDialog_BuildsView checks the modal JSON carries
// the stable IDs, the pre-filled title, the tag options, and the
// metadata.
func TestRealClient_OpenMoveDialog_BuildsView(t *testing.T) {
	s, c := newStubSlack(t, func(apiCall, int) (int, http.Header, string) {
		return ok(`{"ok":true,"view":{"id":"V1"}}`)
	})

	err := c.OpenMoveDialog(context.Background(), "trigger-1", MoveDialog{
		Title:           "Who is building software factories?",
		Tags:            []string{"ai", "architecture", "go"},
		PrivateMetadata: `{"channel":"C1","ts":"1.0"}`,
	})
	if err != nil {
		t.Fatalf("OpenMoveDialog: %v", err)
	}

	var req struct {
		TriggerID string `json:"trigger_id"`
		View      struct {
			Type            string `json:"type"`
			CallbackID      string `json:"callback_id"`
			PrivateMetadata string `json:"private_metadata"`
			Title           struct{ Text string }
			Submit          struct{ Text string }
			Close           struct{ Text string }
			Blocks          []struct {
				Type     string `json:"type"`
				BlockID  string `json:"block_id"`
				Optional bool   `json:"optional"`
				Element  struct {
					Type         string `json:"type"`
					ActionID     string `json:"action_id"`
					InitialValue string `json:"initial_value"`
					MaxLength    int    `json:"max_length"`
					Options      []struct {
						Value string `json:"value"`
						Text  struct{ Text string }
					} `json:"options"`
				} `json:"element"`
				Elements []struct{ Text string } `json:"elements"`
			} `json:"blocks"`
		} `json:"view"`
	}
	if err := json.Unmarshal(s.only("views.open").Body, &req); err != nil {
		t.Fatalf("decode views.open body: %v", err)
	}

	v := req.View
	if req.TriggerID != "trigger-1" || v.Type != "modal" || v.CallbackID != MoveViewCallbackID {
		t.Errorf("trigger/type/callback = %q/%q/%q", req.TriggerID, v.Type, v.CallbackID)
	}
	if v.PrivateMetadata != `{"channel":"C1","ts":"1.0"}` {
		t.Errorf("private_metadata = %q", v.PrivateMetadata)
	}
	if v.Title.Text != "Move to forum" || v.Submit.Text != "Move thread" || v.Close.Text != "Cancel" {
		t.Errorf("title/submit/close = %q/%q/%q", v.Title.Text, v.Submit.Text, v.Close.Text)
	}
	if len(v.Blocks) != 3 {
		t.Fatalf("blocks = %d, want 3", len(v.Blocks))
	}

	title := v.Blocks[0]
	if title.Type != "input" || title.BlockID != MoveTitleBlockID || title.Optional {
		t.Errorf("title block = %+v", title)
	}
	if e := title.Element; e.Type != "plain_text_input" || e.ActionID != MoveTitleActionID ||
		e.InitialValue != "Who is building software factories?" || e.MaxLength != 200 {
		t.Errorf("title element = %+v", e)
	}

	tags := v.Blocks[1]
	if tags.Type != "input" || tags.BlockID != MoveTagsBlockID || tags.Optional {
		t.Errorf("tags block = %+v", tags)
	}
	if e := tags.Element; e.Type != "multi_static_select" || e.ActionID != MoveTagsActionID {
		t.Errorf("tags element = %+v", e)
	}
	var values []string
	for _, o := range tags.Element.Options {
		if o.Text.Text != o.Value {
			t.Errorf("option text %q != value %q", o.Text.Text, o.Value)
		}
		values = append(values, o.Value)
	}
	if want := []string{"ai", "architecture", "go"}; !reflect.DeepEqual(values, want) {
		t.Errorf("tag options = %v, want %v", values, want)
	}

	note := v.Blocks[2]
	wantNote := "The original post and the replies that answer it directly move to the forum under their authors' names. Other replies stay here in Slack."
	if note.Type != "context" || len(note.Elements) != 1 || note.Elements[0].Text != wantNote {
		t.Errorf("note block = %+v", note)
	}
}

// TestRealClient_OpenMoveDialog_LongTitle_TruncatedToMax checks the
// pre-filled title never exceeds the input's maximum, which Slack rejects.
func TestRealClient_OpenMoveDialog_LongTitle_TruncatedToMax(t *testing.T) {
	long := make([]rune, 250)
	for i := range long {
		long[i] = 'é'
	}
	view := moveView(MoveDialog{Title: string(long), Tags: []string{"ai"}})
	body, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshal view: %v", err)
	}
	var decoded struct {
		Blocks []struct {
			Element struct {
				InitialValue string `json:"initial_value"`
			} `json:"element"`
		} `json:"blocks"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode view: %v", err)
	}
	if n := len([]rune(decoded.Blocks[0].Element.InitialValue)); n != 200 {
		t.Errorf("initial title length = %d, want 200", n)
	}
}

// TestRealClient_OpenMoveDialog_NoTags_Refused checks a dialog with no
// tag options is refused before Slack is called.
func TestRealClient_OpenMoveDialog_NoTags_Refused(t *testing.T) {
	s, c := newStubSlack(t, func(apiCall, int) (int, http.Header, string) {
		return ok(`{"ok":true}`)
	})

	if err := c.OpenMoveDialog(context.Background(), "trigger-1", MoveDialog{Title: "x"}); err == nil {
		t.Fatal("OpenMoveDialog with no tags succeeded, want an error")
	}
	if n := len(s.methods()); n != 0 {
		t.Errorf("Slack calls = %d, want 0", n)
	}
}

// TestFake_PostMessage_RecordsPostsAndConsumesErrsInOrder checks scripted
// errors are used one per call and only accepted messages are recorded.
func TestFake_PostMessage_RecordsPostsAndConsumesErrsInOrder(t *testing.T) {
	boom := errors.New("boom")
	f := &Fake{PostErrs: []error{boom, nil, &APIError{Code: "channel_not_found"}}}
	ctx := context.Background()

	if err := f.PostMessage(ctx, "C1", "one", ""); !errors.Is(err, boom) {
		t.Errorf("first call error = %v, want boom", err)
	}
	if err := f.PostMessage(ctx, "C1", "two", "1.0"); err != nil {
		t.Errorf("second call error = %v, want nil", err)
	}
	if err := f.PostMessage(ctx, "C1", "three", ""); ErrorCode(err) != "channel_not_found" {
		t.Errorf("third call error = %v, want channel_not_found", err)
	}
	if err := f.PostMessage(ctx, "U1", "four", ""); err != nil {
		t.Errorf("fourth call error = %v, want nil once PostErrs is used up", err)
	}

	want := []PostedMessage{{Channel: "C1", Text: "two", ThreadTS: "1.0"}, {Channel: "U1", Text: "four"}}
	if got := f.PostedMessages(); !reflect.DeepEqual(got, want) {
		t.Errorf("Posted = %+v, want %+v", got, want)
	}
	if len(f.Calls) != 4 || f.Calls[0].Method != "PostMessage" || f.Calls[0].Args[1] != "one" {
		t.Errorf("Calls = %+v, want all four PostMessage calls", f.Calls)
	}
}

// TestFake_ThreadReplies_ReplyTS_ResolvesToThread checks a reply ts finds
// its thread and an unknown ts is an error.
func TestFake_ThreadReplies_ReplyTS_ResolvesToThread(t *testing.T) {
	thread := []Message{
		{User: "U1", Text: "root", TS: "1.0", ThreadTS: "1.0"},
		{User: "U2", Text: "reply", TS: "2.0", ThreadTS: "1.0"},
	}
	f := &Fake{Threads: map[string][]Message{
		ThreadKey("C1", "1.0"): thread,
		ThreadKey("C2", "5.0"): {{User: "U3", Text: "other", TS: "5.0"}, {User: "U3", Text: "same ts elsewhere", TS: "2.0"}},
	}}
	ctx := context.Background()

	for _, ts := range []string{"1.0", "2.0"} {
		got, err := f.ThreadReplies(ctx, "C1", ts)
		if err != nil {
			t.Fatalf("ThreadReplies %s: %v", ts, err)
		}
		if !reflect.DeepEqual(got, thread) {
			t.Errorf("ThreadReplies %s = %+v, want the C1 thread", ts, got)
		}
	}
	if _, err := f.ThreadReplies(ctx, "C1", "9.0"); ErrorCode(err) != "thread_not_found" {
		t.Errorf("unknown ts error = %v, want thread_not_found", err)
	}
}

// TestFake_Lookups_ScriptedAndMissing checks channel, user, permalink and
// dialog scripting.
func TestFake_Lookups_ScriptedAndMissing(t *testing.T) {
	f := &Fake{
		Channels:   map[string]Channel{"C1": {ID: "C1", Name: "general", IsPublic: true}},
		Users:      map[string]User{"U1": {ID: "U1", Name: "Dana"}},
		Permalinks: map[string]string{ThreadKey("C1", "1.0"): "https://forge.slack.test/p1"},
	}
	ctx := context.Background()

	if ch, err := f.ChannelInfo(ctx, "C1"); err != nil || !ch.IsPublic {
		t.Errorf("ChannelInfo C1 = %+v, %v", ch, err)
	}
	if _, err := f.ChannelInfo(ctx, "C404"); ErrorCode(err) != "channel_not_found" {
		t.Errorf("ChannelInfo C404 error = %v", err)
	}
	if u, err := f.UserInfo(ctx, "U1"); err != nil || u.Name != "Dana" {
		t.Errorf("UserInfo U1 = %+v, %v", u, err)
	}
	if _, err := f.UserInfo(ctx, "U404"); ErrorCode(err) != "user_not_found" {
		t.Errorf("UserInfo U404 error = %v", err)
	}
	if link, _ := f.Permalink(ctx, "C1", "1.0"); link != "https://forge.slack.test/p1" {
		t.Errorf("scripted permalink = %q", link)
	}
	if link, err := f.Permalink(ctx, "C1", "2.5"); err != nil || link != "https://slack.test/archives/C1/p25" {
		t.Errorf("default permalink = %q, %v", link, err)
	}

	d := MoveDialog{Title: "t", Tags: []string{"ai"}, PrivateMetadata: "m"}
	if err := f.OpenMoveDialog(ctx, "trig", d); err != nil {
		t.Fatalf("OpenMoveDialog: %v", err)
	}
	if len(f.Dialogs) != 1 || f.Dialogs[0].TriggerID != "trig" || !reflect.DeepEqual(f.Dialogs[0].Dialog, d) {
		t.Errorf("Dialogs = %+v", f.Dialogs)
	}
}

// TestFake_ConcurrentPosts_AllRecorded checks the fake is safe to share
// between goroutines (run with -race).
func TestFake_ConcurrentPosts_AllRecorded(t *testing.T) {
	f := &Fake{}
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			f.PostMessage(context.Background(), "C1", "hi", "")
		})
	}
	wg.Wait()
	if n := len(f.PostedMessages()); n != 50 {
		t.Errorf("Posted = %d, want 50", n)
	}
}
