package slackx

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const passThroughToken = "forum-service-token-0123456789abcdef"

// passThroughCall is one request the stand-in for forge-bot accepted.
type passThroughCall struct {
	SlackMethod string
	ContentType string
	Form        url.Values
}

// newPassThrough starts a stand-in for forge-bot's Slack API pass-through,
// as strict as the service contract allows it to be: only POST, only under
// /slack/api/, only with the service token as the bearer token.
func newPassThrough(t *testing.T) (baseURL string, calls func() []passThroughCall) {
	t.Helper()
	answers := map[string]string{
		"chat.postMessage":      `{"ok":true,"channel":"C1","ts":"2.0"}`,
		"conversations.replies": `{"ok":true,"messages":[{"type":"message","user":"U1","text":"root","ts":"1.0","thread_ts":"1.0"}]}`,
		"conversations.info":    `{"ok":true,"channel":{"id":"C1","name":"kubernetes","is_channel":true}}`,
		"conversations.join":    `{"ok":true,"channel":{"id":"C1"}}`,
		"users.info":            `{"ok":true,"user":{"id":"U1","name":"ada","profile":{"display_name":"Ada"}}}`,
		"chat.getPermalink":     `{"ok":true,"channel":"C1","permalink":"https://forge.slack.test/archives/C1/p10"}`,
		"views.open":            `{"ok":true,"view":{"id":"V1"}}`,
	}
	var mu sync.Mutex
	var got []passThroughCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+passThroughToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		method, found := strings.CutPrefix(r.URL.Path, "/slack/api/")
		answer, known := answers[method]
		if !found || !known {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Method != http.MethodPost || r.URL.RawQuery != "" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		body, _ := io.ReadAll(r.Body)
		c := passThroughCall{SlackMethod: method, ContentType: r.Header.Get("Content-Type")}
		if c.ContentType == "application/x-www-form-urlencoded" {
			c.Form, _ = url.ParseQuery(string(body))
		}
		mu.Lock()
		got = append(got, c)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, answer)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/slack/api/", func() []passThroughCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]passThroughCall(nil), got...)
	}
}

// TestRealClient_AllCalls_PassThroughBaseURL_ReachBotAsContractPosts checks every call the forum makes arrives at <bot>/slack/api/<method> as a POST with the service token as bearer.
func TestRealClient_AllCalls_PassThroughBaseURL_ReachBotAsContractPosts(t *testing.T) {
	// The base URL is accepted with or without its trailing slash.
	for _, trim := range []bool{false, true} {
		baseURL, calls := newPassThrough(t)
		if trim {
			baseURL = strings.TrimSuffix(baseURL, "/")
		}
		c := NewClient(passThroughToken, baseURL, nil)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := c.PostMessage(ctx, "C1", "hello", "1.0"); err != nil {
			t.Errorf("PostMessage: %v", err)
		}
		if msgs, err := c.ThreadReplies(ctx, "C1", "1.0"); err != nil || len(msgs) != 1 {
			t.Errorf("ThreadReplies = %v, %v", msgs, err)
		}
		if root, err := c.ThreadRoot(ctx, "C1", "1.0"); err != nil || root.Text != "root" {
			t.Errorf("ThreadRoot = %+v, %v", root, err)
		}
		if ch, err := c.ChannelInfo(ctx, "C1"); err != nil || !ch.IsPublic {
			t.Errorf("ChannelInfo = %+v, %v", ch, err)
		}
		if u, err := c.UserInfo(ctx, "U1"); err != nil || u.Name != "Ada" {
			t.Errorf("UserInfo = %+v, %v", u, err)
		}
		link, err := c.Permalink(ctx, "C1", "1.0")
		if err != nil || link != "https://forge.slack.test/archives/C1/p10" {
			t.Errorf("Permalink = %q, %v", link, err)
		}
		if err := c.OpenMoveDialog(ctx, "trigger-1", MoveDialog{Title: "A title", Tags: []string{"go"}}); err != nil {
			t.Errorf("OpenMoveDialog: %v", err)
		}

		const form, jsonBody = "application/x-www-form-urlencoded", "application/json"
		want := []passThroughCall{
			{SlackMethod: "chat.postMessage", ContentType: form},
			{SlackMethod: "conversations.replies", ContentType: form},
			{SlackMethod: "conversations.replies", ContentType: form},
			{SlackMethod: "conversations.info", ContentType: form},
			{SlackMethod: "users.info", ContentType: form},
			{SlackMethod: "chat.getPermalink", ContentType: form},
			{SlackMethod: "views.open", ContentType: jsonBody},
		}
		got := calls()
		var permalink url.Values
		for i := range got {
			if got[i].SlackMethod == "chat.getPermalink" {
				permalink = got[i].Form
			}
			got[i].Form = nil
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("trailing slash trimmed %v: calls = %+v\nwant %+v", trim, got, want)
		}
		if permalink.Get("channel") != "C1" || permalink.Get("message_ts") != "1.0" {
			t.Errorf("chat.getPermalink form = %v, want channel C1 and message_ts 1.0", permalink)
		}
	}
}

// TestRealClient_PostMessage_WrongServiceToken_Fails checks the bot's 401 for an unknown token surfaces as an error.
func TestRealClient_PostMessage_WrongServiceToken_Fails(t *testing.T) {
	baseURL, calls := newPassThrough(t)
	c := NewClient("not-the-service-token", baseURL, nil)
	if err := c.PostMessage(context.Background(), "C1", "hello", ""); err == nil {
		t.Error("PostMessage with a token the bot does not know succeeded")
	}
	if n := len(calls()); n != 0 {
		t.Errorf("the bot accepted %d calls, want none", n)
	}
}

// TestRealClient_JoinChannel_PassThroughBaseURL_ReachesBot checks the join made after not_in_channel also goes through the bot.
func TestRealClient_JoinChannel_PassThroughBaseURL_ReachesBot(t *testing.T) {
	baseURL, calls := newPassThrough(t)
	c := NewClient(passThroughToken, baseURL, nil)
	if _, _, _, err := c.api.JoinConversationContext(context.Background(), "C1"); err != nil {
		t.Fatalf("conversations.join: %v", err)
	}
	if got := calls(); len(got) != 1 || got[0].SlackMethod != "conversations.join" || got[0].Form.Get("channel") != "C1" {
		t.Errorf("calls = %+v, want one conversations.join for C1", got)
	}
}
