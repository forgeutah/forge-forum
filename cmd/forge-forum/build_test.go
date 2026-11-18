package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/forgeutah/forge-forum/internal/config"
	"github.com/forgeutah/forge-forum/internal/db"
)

const buildTestSecret = "build-test-secret-build-test-secret"

func signed(method, path string, form url.Values) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	r.Header.Set("X-Forge-Proxy-Secret", buildTestSecret)
	r.Header.Set("X-Forge-Contract-Version", "1")
	r.Header.Set("X-Forge-User-Id", "7")
	r.Header.Set("X-Forge-Name", "Sam R")
	r.Header.Set("X-Forge-Slack-User-Id", "U0SAM")
	r.Header.Set("X-Forge-Slack-Team-Id", "T0FORGE")
	return r
}

// TestBuild_SlackOff_PostSucceedsAndOutboxRowIsSkipped checks the wiring
// with Slack unconfigured: it is logged once, a thread posts, and the
// started worker is woken and marks the announcement skipped.
func TestBuild_SlackOff_PostSucceedsAndOutboxRowIsSkipped(t *testing.T) {
	database, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "forum.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	var logs bytes.Buffer
	cfg := config.Config{
		ProxySecrets:  []string{buildTestSecret},
		SlackTeamID:   "T0FORGE",
		AdminRole:     "admin",
		PublicBaseURL: "https://forum.example.com",
	}
	handler, start, _, err := build(ctx, cfg, database, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if n := strings.Count(logs.String(), "Slack is not configured"); n != 1 {
		t.Errorf("Slack-off notice logged %d times, want once", n)
	}
	start()

	do := func(r *http.Request) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if w := do(httptest.NewRequest(http.MethodGet, "/healthz", nil)); w.Code != http.StatusOK {
		t.Errorf("GET /healthz: status %d", w.Code)
	}
	if w := do(httptest.NewRequest(http.MethodGet, "/", nil)); w.Code != http.StatusUnauthorized {
		t.Errorf("GET / without the proxy contract: status %d, want 401", w.Code)
	}

	token := regexp.MustCompile(`name="csrf_token" value="([^"]+)"`).FindStringSubmatch(do(signed(http.MethodGet, "/threads/new", nil)).Body.String())
	if token == nil {
		t.Fatal("no CSRF token on the new-thread form")
	}
	w := do(signed(http.MethodPost, "/threads", url.Values{
		"csrf_token": {token[1]}, "title": {"Hello"}, "body": {"Body"}, "tags": {"general"},
	}))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("POST /threads: status %d", w.Code)
	}

	// The post woke the worker; it should settle the row well inside its
	// polling interval.
	deadline := time.Now().Add(3 * time.Second)
	var kind, status string
	for {
		err := database.Reader().QueryRow(`SELECT kind, status FROM notifications`).Scan(&kind, &status)
		if err == nil && status != "pending" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("outbox row not settled: kind %q status %q err %v", kind, status, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if kind != "announce" || status != "skipped" {
		t.Errorf("outbox row = %s/%s, want announce/skipped", kind, status)
	}
}

// TestBuild_Stop_ReturnsInTimeWithBackgroundWorkEnded checks the stop
// function ends the outbox worker and waits for it, inside its deadline,
// so the database can be closed behind it.
func TestBuild_Stop_ReturnsInTimeWithBackgroundWorkEnded(t *testing.T) {
	database, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "forum.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	var logs bytes.Buffer
	cfg := config.Config{
		ProxySecrets:  []string{buildTestSecret},
		SlackTeamID:   "T0FORGE",
		AdminRole:     "admin",
		PublicBaseURL: "https://forum.example.com",
	}
	// The context build is given stays live: stopping is the stop
	// function's job, not the caller's.
	_, start, stop, err := build(context.Background(), cfg, database, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	start()

	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stop(stopCtx)
	if err := stopCtx.Err(); err != nil {
		t.Fatalf("stop used up its deadline: %v", err)
	}
	// The worker's goroutine logs this as its last act; stop returned
	// after it, so nothing is left running against the database.
	if n := strings.Count(logs.String(), "outbox worker stopped"); n != 1 {
		t.Errorf("worker stop logged %d times, want once:\n%s", n, logs.String())
	}
	if strings.Contains(logs.String(), "did not stop in time") {
		t.Errorf("stop gave up waiting:\n%s", logs.String())
	}

	stop(stopCtx) // a second call has nothing left to wait for
}

// TestBuild_Stop_BeforeStart_ReturnsAtOnce checks stopping an application
// whose background work never started does not wait for anything.
func TestBuild_Stop_BeforeStart_ReturnsAtOnce(t *testing.T) {
	database, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "forum.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	cfg := config.Config{ProxySecrets: []string{buildTestSecret}, SlackTeamID: "T0FORGE", AdminRole: "admin", PublicBaseURL: "https://forum.example.com"}
	var logs bytes.Buffer
	_, _, stop, err := build(context.Background(), cfg, database, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stop(stopCtx)
	if err := stopCtx.Err(); err != nil || strings.Contains(logs.String(), "did not stop in time") {
		t.Errorf("stop before start: ctx %v, logs:\n%s", err, logs.String())
	}
}

const (
	buildTestBotToken = "forum-service-token-0123456789abcdef"

	// A message shortcut as forge-bot forwards it (service contract 1).
	buildTestShortcut = `{
		"type": "message_shortcut",
		"callback_id": "move_to_forum",
		"trigger_id": "9001.43.abcdef",
		"team_id": "T0FORGE",
		"user": {"id": "UMOVER", "name": "mo"},
		"channel": {"id": "C1"},
		"message": {"ts": "1760950800.000100", "thread_ts": "", "user": "UASKER", "bot_id": "", "subtype": "",
			"text": "Has anyone moved from Ingress to Gateway API?"}
	}`
)

// botCall is one Slack API call the stand-in for forge-bot received.
type botCall struct {
	Path, Authorization, Body string
}

// newBotStub stands in for forge-bot's Slack API pass-through. It answers
// the two calls a shortcut on a public channel's message makes.
func newBotStub(t *testing.T) (baseURL string, calls func() []botCall) {
	t.Helper()
	var mu sync.Mutex
	var got []botCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, botCall{Path: r.URL.Path, Authorization: r.Header.Get("Authorization"), Body: string(body)})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/slack/api/conversations.info":
			io.WriteString(w, `{"ok":true,"channel":{"id":"C1","name":"kubernetes","is_channel":true}}`)
		case "/slack/api/views.open":
			io.WriteString(w, `{"ok":true,"view":{"id":"V1"}}`)
		default:
			io.WriteString(w, `{"ok":false,"error":"unknown_method"}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() []botCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]botCall(nil), got...)
	}
}

// buildForInteractions builds the application over a fresh database and
// returns a function that serves one request.
func buildForInteractions(t *testing.T, cfg config.Config) func(*http.Request) *httptest.ResponseRecorder {
	t.Helper()
	database, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "forum.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	cfg.ProxySecrets = []string{buildTestSecret}
	cfg.SlackTeamID = "T0FORGE"
	cfg.AdminRole = "admin"
	cfg.PublicBaseURL = "https://forum.example.com"
	handler, _, stop, err := build(context.Background(), cfg, database, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		stop(stopCtx)
	})
	return func(r *http.Request) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
}

// interaction is a POST to the interactions endpoint; bearer "" sends no
// Authorization header.
func interaction(bearer, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/internal/slack/interactions", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	return r
}

// TestBuild_MoveOn_AuthenticatedInteractionReachesMover checks a shortcut
// forwarded with the service token, and no proxy headers, runs the mover,
// which opens the dialog through the bot's Slack API with the same token.
func TestBuild_MoveOn_AuthenticatedInteractionReachesMover(t *testing.T) {
	botURL, calls := newBotStub(t)
	do := buildForInteractions(t, config.Config{
		ForgeBotURL:     botURL,
		ForgeBotToken:   buildTestBotToken,
		AnthropicAPIKey: "sk-ant-not-a-real-key", // a shortcut never reaches the classifier
		ClassifierModel: "claude-haiku-5-5",
	})

	w := do(interaction(buildTestBotToken, buildTestShortcut))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q; want 200", w.Code, w.Body.String())
	}
	got := calls()
	if len(got) != 2 || got[0].Path != "/slack/api/conversations.info" || got[1].Path != "/slack/api/views.open" {
		t.Fatalf("Slack API calls through the bot = %+v, want conversations.info then views.open", got)
	}
	for _, c := range got {
		if c.Authorization != "Bearer "+buildTestBotToken {
			t.Errorf("%s Authorization = %q, want the service token", c.Path, c.Authorization)
		}
	}
	for _, want := range []string{`"trigger_id":"9001.43.abcdef"`, `"callback_id":"move_to_forum_submit"`, "Has anyone moved from Ingress to Gateway API?"} {
		if !strings.Contains(got[1].Body, want) {
			t.Errorf("views.open body missing %s:\n%s", want, got[1].Body)
		}
	}
}

// TestBuild_MoveOn_InteractionWithoutToken_Unauthorized checks the bearer
// token is the only way in, even for a signed-in member behind the proxy.
func TestBuild_MoveOn_InteractionWithoutToken_Unauthorized(t *testing.T) {
	botURL, calls := newBotStub(t)
	do := buildForInteractions(t, config.Config{
		ForgeBotURL:     botURL,
		ForgeBotToken:   buildTestBotToken,
		AnthropicAPIKey: "sk-ant-not-a-real-key",
		ClassifierModel: "claude-haiku-5-5",
	})

	member := signed(http.MethodPost, "/internal/slack/interactions", nil)
	member.Body = io.NopCloser(strings.NewReader(buildTestShortcut))
	member.Header.Set("Content-Type", "application/json")
	requests := map[string]*http.Request{
		"no token":               interaction("", buildTestShortcut),
		"wrong token":            interaction(strings.Repeat("x", len(buildTestBotToken)), buildTestShortcut),
		"the proxy secret":       interaction(buildTestSecret, buildTestShortcut),
		"a member, no token":     member,
		"a GET without a token":  httptest.NewRequest(http.MethodGet, "/internal/slack/interactions", nil),
		"a HEAD without a token": httptest.NewRequest(http.MethodHead, "/internal/slack/interactions", nil),
	}
	for name, r := range requests {
		if w := do(r); w.Code != http.StatusUnauthorized || strings.TrimSpace(w.Body.String()) != "" {
			t.Errorf("%s: status = %d, body %q; want a bare 401", name, w.Code, w.Body.String())
		}
	}
	if got := calls(); len(got) != 0 {
		t.Errorf("unauthenticated interactions reached Slack: %+v", got)
	}
}

// TestBuild_MoveOff_InteractionsNotFound checks the endpoint does not
// exist when Move to forum is off, whatever token is sent.
func TestBuild_MoveOff_InteractionsNotFound(t *testing.T) {
	configs := map[string]config.Config{
		"Slack off":                {},
		"Slack on, no classifier":  {ForgeBotURL: "http://forge-bot.test:8080", ForgeBotToken: buildTestBotToken},
		"classifier key, no Slack": {AnthropicAPIKey: "sk-ant-not-a-real-key", ClassifierModel: "claude-haiku-5-5"},
	}
	for name, cfg := range configs {
		do := buildForInteractions(t, cfg)
		for _, bearer := range []string{"", buildTestBotToken} {
			if w := do(interaction(bearer, buildTestShortcut)); w.Code != http.StatusNotFound {
				t.Errorf("%s, bearer %q: status = %d, want 404", name, bearer, w.Code)
			}
		}
	}
}
