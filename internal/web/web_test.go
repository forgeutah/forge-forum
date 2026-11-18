package web

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/forgeutah/forge-forum/internal/config"
	"github.com/forgeutah/forge-forum/internal/db"
	"github.com/forgeutah/forge-forum/internal/forum"
)

const (
	testSecret = "web-test-secret-web-test-secret-web"
	testTeam   = "T0FORGE"
)

// user is a signed-in member as forge-proxy would describe them.
type user struct {
	slackID, name, avatar, roles string
}

var (
	sam   = user{slackID: "U0SAM", name: "Sam R"}
	priya = user{slackID: "U0PRIYA", name: "Priya N", avatar: "https://avatars.example.com/priya.png"}
)

// app is the real handler chain (identity middleware, CSRF, handlers) over
// a real SQLite database in a temp dir.
type app struct {
	t     *testing.T
	h     http.Handler
	db    *db.DB
	store *forum.Store
	now   time.Time
}

func newApp(t *testing.T, mutate ...func(*config.Config)) *app {
	t.Helper()
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "forum.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	cfg := config.Config{
		ProxySecrets:  []string{testSecret},
		SlackTeamID:   testTeam,
		AdminRole:     "admin",
		PublicBaseURL: "https://forum.example.com",
	}
	for _, m := range mutate {
		m(&cfg)
	}
	a := &app{t: t, db: d, now: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)}
	// Each call to the clock moves one second on, so posts made in order
	// have ordered timestamps.
	a.store = forum.NewStore(d, func() time.Time {
		a.now = a.now.Add(time.Second)
		return a.now
	})
	srv, err := New(cfg, d, a.store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a.h = srv.Handler()
	return a
}

func (u user) sign(r *http.Request) {
	r.Header.Set("X-Forge-Proxy-Secret", testSecret)
	r.Header.Set("X-Forge-Contract-Version", "1")
	r.Header.Set("X-Forge-User-Id", fmt.Sprint(len(u.slackID)+100))
	r.Header.Set("X-Forge-Name", u.name)
	r.Header.Set("X-Forge-Avatar", u.avatar)
	r.Header.Set("X-Forge-Roles", u.roles)
	r.Header.Set("X-Forge-Slack-User-Id", u.slackID)
	r.Header.Set("X-Forge-Slack-Team-Id", testTeam)
}

func (a *app) do(r *http.Request) *httptest.ResponseRecorder {
	a.t.Helper()
	w := httptest.NewRecorder()
	a.h.ServeHTTP(w, r)
	return w
}

func (a *app) get(u user, path string) *httptest.ResponseRecorder {
	a.t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	u.sign(r)
	return a.do(r)
}

func (a *app) post(u user, path string, form url.Values) *httptest.ResponseRecorder {
	a.t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	u.sign(r)
	return a.do(r)
}

var csrfInput = regexp.MustCompile(`name="csrf_token" value="([^"]+)"`)

// token reads the CSRF token the new-thread form gives this member.
func (a *app) token(u user) string {
	a.t.Helper()
	w := a.get(u, "/threads/new")
	m := csrfInput.FindStringSubmatch(w.Body.String())
	if w.Code != http.StatusOK || m == nil {
		a.t.Fatalf("GET /threads/new: status %d, no CSRF token in form", w.Code)
	}
	return m[1]
}

// createThread posts the new-thread form and returns the thread's path.
func (a *app) createThread(u user, title, body string, tags ...string) string {
	a.t.Helper()
	w := a.post(u, "/threads", url.Values{
		"csrf_token": {a.token(u)}, "title": {title}, "body": {body}, "tags": tags,
	})
	if w.Code != http.StatusSeeOther {
		a.t.Fatalf("POST /threads %q: status %d, body: %s", title, w.Code, w.Body.String())
	}
	return w.Header().Get("Location")
}

func (a *app) exec(query string, args ...any) {
	a.t.Helper()
	err := a.db.WithWriteTx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(query, args...)
		return err
	})
	if err != nil {
		a.t.Fatalf("exec %q: %v", query, err)
	}
}

func (a *app) threadCount() int {
	a.t.Helper()
	var n int
	if err := a.db.Reader().QueryRow(`SELECT COUNT(*) FROM threads`).Scan(&n); err != nil {
		a.t.Fatalf("count threads: %v", err)
	}
	return n
}

func wantContains(t *testing.T, body string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(body, want) {
			t.Errorf("page does not contain %q", want)
		}
	}
}

func wantNotContains(t *testing.T, body string, bads ...string) {
	t.Helper()
	for _, bad := range bads {
		if strings.Contains(body, bad) {
			t.Errorf("page contains %q", bad)
		}
	}
}

// wantOrder checks that the given strings appear in the page in order.
func wantOrder(t *testing.T, body string, parts ...string) {
	t.Helper()
	at := 0
	for _, p := range parts {
		i := strings.Index(body[at:], p)
		if i < 0 {
			t.Errorf("%q missing or out of order (wanted order %q)", p, parts)
			return
		}
		at += i + len(p)
	}
}

// TestServer_NoProxyContract_401NoContent covers AE1 through the full
// chain: without the proxy secret no page, form, or asset is served.
func TestServer_NoProxyContract_401NoContent(t *testing.T) {
	a := newApp(t)
	thread := a.createThread(sam, "Secret planning thread", "Members only body", "general")

	for _, path := range []string{"/", thread, "/threads/new", "/assets/forum.css", "/no-such-page"} {
		w := a.do(httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without contract: status %d, want 401", path, w.Code)
		}
		wantNotContains(t, w.Body.String(), "Secret planning thread", "Members only body", "Forge Forum", "csrf_token", "--accent")
	}

	// Right secret, wrong workspace.
	r := httptest.NewRequest(http.MethodGet, thread, nil)
	sam.sign(r)
	r.Header.Set("X-Forge-Slack-Team-Id", "T0OTHER")
	if w := a.do(r); w.Code != http.StatusUnauthorized || strings.Contains(w.Body.String(), "Secret planning") {
		t.Errorf("wrong team: status %d", w.Code)
	}

	// A post without the contract stores nothing.
	r = httptest.NewRequest(http.MethodPost, "/threads", strings.NewReader(url.Values{
		"csrf_token": {a.token(sam)}, "title": {"x"}, "body": {"y"}, "tags": {"go"}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if w := a.do(r); w.Code != http.StatusUnauthorized || a.threadCount() != 1 {
		t.Errorf("unsigned post: status %d, threads %d", w.Code, a.threadCount())
	}
}

// TestServer_CreateThread_TwoTags_TopOfFeedWithBothTags checks the main
// flow: form, post, redirect, thread page, and the feed's first row.
func TestServer_CreateThread_TwoTags_TopOfFeedWithBothTags(t *testing.T) {
	a := newApp(t)
	a.createThread(priya, "An older thread", "Body", "general")
	loc := a.createThread(sam, "Who is building software factories?", "Is anyone here running one?", "ai", "architecture")
	if !regexp.MustCompile(`^/threads/\d+$`).MatchString(loc) {
		t.Fatalf("redirect = %q, want /threads/<id>", loc)
	}

	feed := a.get(priya, "/")
	if feed.Code != http.StatusOK {
		t.Fatalf("feed status = %d", feed.Code)
	}
	body := feed.Body.String()
	wantContains(t, body, "<h1", "Forge Forum", "&gt; threads",
		"Replies nest under what they answer. The most upvoted rise to the top.",
		"recent activity", "most upvoted", `href="`+loc+`"`,
		"Members only. Signed in with your Forge Utah Slack account.")
	wantOrder(t, body, `<div class="thread-list"`, "Who is building software factories?", "#ai", "#architecture", "@Sam R", "An older thread")

	page := a.get(priya, loc)
	if page.Code != http.StatusOK {
		t.Fatalf("thread status = %d", page.Code)
	}
	wantContains(t, page.Body.String(), "Who is building software factories?", "Is anyone here running one?", "#ai", "#architecture", "@Sam R", "back to feed")
}

// TestServer_CreateThread_Invalid_RejectedWithMessage checks that a thread
// with no tag, an unknown tag, or an empty title or body is refused with a
// message, keeps what the member typed, and stores nothing.
func TestServer_CreateThread_Invalid_RejectedWithMessage(t *testing.T) {
	cases := []struct {
		name    string
		form    url.Values
		message string
	}{
		{"no tag", url.Values{"title": {"Kept title"}, "body": {"Kept body"}}, "Pick at least one tag."},
		{"unknown tag", url.Values{"title": {"Kept title"}, "body": {"Kept body"}, "tags": {"go", "nonsense"}}, "not on the list"},
		{"empty title", url.Values{"title": {"  "}, "body": {"Kept body"}, "tags": {"go"}}, "Give the thread a title."},
		{"empty body", url.Values{"title": {"Kept title"}, "body": {""}, "tags": {"go"}}, "Write a body"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newApp(t)
			tc.form.Set("csrf_token", a.token(sam))
			w := a.post(sam, "/threads", tc.form)
			if w.Code != http.StatusUnprocessableEntity {
				t.Errorf("status = %d, want 422", w.Code)
			}
			body := w.Body.String()
			wantContains(t, body, tc.message, `role="alert"`, "Start a discussion")
			if v := tc.form.Get("title"); strings.TrimSpace(v) != "" {
				wantContains(t, body, `value="Kept title"`)
			}
			if v := tc.form.Get("body"); v != "" {
				wantContains(t, body, ">Kept body</textarea>")
			}
			if tc.form.Has("tags") {
				wantContains(t, body, `value="go" checked`)
			}
			if n := a.threadCount(); n != 0 {
				t.Errorf("threads stored = %d, want 0", n)
			}
		})
	}
}

// TestServer_Post_BadCSRF_Rejected checks that a state-changing post needs
// the signed-in member's own token.
func TestServer_Post_BadCSRF_Rejected(t *testing.T) {
	a := newApp(t)
	good := url.Values{"title": {"Title"}, "body": {"Body"}, "tags": {"go"}}
	cases := map[string]string{
		"missing":        "",
		"garbage":        "not-a-token",
		"another member": a.token(priya),
		"truncated":      a.token(sam)[:20],
	}
	for name, token := range cases {
		t.Run(name, func(t *testing.T) {
			form := url.Values{}
			for k, v := range good {
				form[k] = v
			}
			if token != "" {
				form.Set("csrf_token", token)
			}
			w := a.post(sam, "/threads", form)
			if w.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403", w.Code)
			}
			if n := a.threadCount(); n != 0 {
				t.Errorf("threads stored = %d, want 0", n)
			}
		})
	}
	// The same form with the member's own token goes through.
	good.Set("csrf_token", a.token(sam))
	if w := a.post(sam, "/threads", good); w.Code != http.StatusSeeOther {
		t.Errorf("valid token: status = %d, want 303", w.Code)
	}
}

// TestServer_Thread_ScriptAndFencedCode_RendersCodeNoScript checks the
// body goes through Markdown rendering and sanitizing on the thread page.
func TestServer_Thread_ScriptAndFencedCode_RendersCodeNoScript(t *testing.T) {
	a := newApp(t)
	md := "Look at **this**:\n\n<script>alert('pwn')</script>\n\n```yaml\nrequire: [acceptance_test, owner]\n```\n"
	loc := a.createThread(sam, "Code sample", md, "go")
	body := a.get(sam, loc).Body.String()
	wantContains(t, body, "<strong>this</strong>", "<pre><code", "require: [acceptance_test, owner]")
	wantNotContains(t, body, "<script", "alert('pwn')")
}

// TestServer_Title_HTMLEscapedEverywhere checks that markup in a title is
// shown as text on the feed and the thread page, including the <title>.
func TestServer_Title_HTMLEscapedEverywhere(t *testing.T) {
	a := newApp(t)
	loc := a.createThread(sam, `<img src=x onerror=alert(1)> & "quotes"`, "Body", "go")
	for _, path := range []string{"/", loc} {
		body := a.get(sam, path).Body.String()
		wantContains(t, body, "&lt;img src=x onerror=alert(1)&gt; &amp; &#34;quotes&#34;")
		wantNotContains(t, body, "<img src=x")
	}
}

// TestServer_Feed_ActivityAndUpvoteOrder checks the two orderings and the
// sort links. Replies and votes arrive in a later unit, so the columns
// they maintain are written directly.
func TestServer_Feed_ActivityAndUpvoteOrder(t *testing.T) {
	a := newApp(t)
	a.createThread(sam, "Thread Alpha", "Body", "go")
	a.createThread(sam, "Thread Bravo", "Body", "go")
	a.createThread(sam, "Thread Charlie", "Body", "go")

	rows := `<div class="thread-list"`
	wantOrder(t, a.get(sam, "/").Body.String(), rows, "Thread Charlie", "Thread Bravo", "Thread Alpha")

	a.exec(`UPDATE threads SET reply_count = 1, last_activity_at = ? WHERE title = 'Thread Alpha'`, a.store.Now().Unix())
	body := a.get(sam, "/?sort=activity").Body.String()
	wantOrder(t, body, rows, "Thread Alpha", "1 reply", "last reply", "Thread Charlie", "Thread Bravo")
	wantContains(t, body, `href="/?sort=top"`)

	a.exec(`UPDATE threads SET score = 7 WHERE title = 'Thread Bravo'`)
	a.exec(`UPDATE threads SET score = 3 WHERE title = 'Thread Alpha'`)
	body = a.get(sam, "/?sort=top").Body.String()
	wantOrder(t, body, rows, "Thread Bravo", "Thread Alpha", "Thread Charlie")
	wantContains(t, body, `aria-label="7 upvotes"`, `aria-label="3 upvotes"`, `aria-label="0 upvotes"`)

	// An unknown sort value falls back to recent activity.
	wantOrder(t, a.get(sam, "/?sort=bogus").Body.String(), rows, "Thread Alpha", "Thread Charlie", "Thread Bravo")
}

// TestServer_Feed_TagFilter_OnlyThreadsCarryingIt checks the filter, that
// it survives a change of sort, and that the active tag is marked.
func TestServer_Feed_TagFilter_OnlyThreadsCarryingIt(t *testing.T) {
	a := newApp(t)
	a.createThread(sam, "Thread about Go", "Body", "go")
	a.createThread(sam, "Thread about agents", "Body", "ai", "go")
	a.createThread(sam, "Thread about jobs", "Body", "careers")

	body := a.get(sam, "/?tag=go").Body.String()
	wantContains(t, body, "Thread about Go", "Thread about agents", `href="/?sort=top&amp;tag=go"`, `href="/?tag=go" aria-current="true"`)
	wantNotContains(t, body, "Thread about jobs")

	body = a.get(sam, "/?tag=kubernetes").Body.String()
	wantNotContains(t, body, "Thread about")
	wantContains(t, body, "No threads tagged #kubernetes yet.")

	// A tag that does not exist is an empty feed, not an error.
	if w := a.get(sam, "/?tag=%3Cscript%3E"); w.Code != http.StatusOK || strings.Contains(w.Body.String(), "<script>") {
		t.Errorf("unknown tag: status %d", w.Code)
	}
}

// TestServer_Feed_Pagination checks page links, the last page, and a page
// past the end.
func TestServer_Feed_Pagination(t *testing.T) {
	a := newApp(t)
	for i := 1; i <= pageSize+3; i++ {
		a.createThread(sam, fmt.Sprintf("Numbered thread %02d.", i), "Body", "go")
	}
	first := fmt.Sprintf("Numbered thread %02d.", pageSize+3)

	body := a.get(sam, "/?tag=go").Body.String()
	wantContains(t, body, first, "Numbered thread 04.", `href="/?page=2&amp;tag=go"`)
	wantNotContains(t, body, "Numbered thread 03.", "newer")

	body = a.get(sam, "/?tag=go&page=2").Body.String()
	wantContains(t, body, "Numbered thread 03.", "Numbered thread 01.", `href="/?tag=go"`, "newer")
	wantNotContains(t, body, "Numbered thread 04.", "older")

	for _, page := range []string{"9", "-1", "abc", "99999999999999999999"} {
		w := a.get(sam, "/?page="+page)
		if w.Code != http.StatusOK {
			t.Errorf("page=%s: status %d, want 200", page, w.Code)
		}
	}
	body = a.get(sam, "/?page=9").Body.String()
	wantNotContains(t, body, "Numbered thread")
	wantContains(t, body, "No more threads on this page.")
}

// TestServer_Feed_Empty_InvitesFirstThread checks the empty state.
func TestServer_Feed_Empty_InvitesFirstThread(t *testing.T) {
	a := newApp(t)
	w := a.get(sam, "/")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	wantContains(t, w.Body.String(), "No threads yet.", `<a href="/threads/new">Start the first one</a>`)
}

// TestServer_DeletedThread_GoneFromFeedAndPage checks soft deletion (plan
// KTD12): the feed drops the thread and its URL shows a removed notice
// with none of its content.
func TestServer_DeletedThread_GoneFromFeedAndPage(t *testing.T) {
	a := newApp(t)
	loc := a.createThread(sam, "Regrettable thread", "Regrettable body", "go")
	a.exec(`UPDATE threads SET deleted_at = 1`)

	wantNotContains(t, a.get(sam, "/").Body.String(), "Regrettable")
	w := a.get(sam, loc)
	if w.Code != http.StatusGone {
		t.Errorf("status = %d, want 410", w.Code)
	}
	wantContains(t, w.Body.String(), "This thread was removed.")
	wantNotContains(t, w.Body.String(), "Regrettable")
}

// TestServer_UnknownPaths_404 checks missing threads, malformed IDs, and
// unrouted paths all get the not-found page.
func TestServer_UnknownPaths_404(t *testing.T) {
	a := newApp(t)
	for _, path := range []string{"/threads/999", "/threads/abc", "/threads/-1", "/nope", "/assets/nope.css", "/assets/"} {
		w := a.get(sam, path)
		if w.Code != http.StatusNotFound {
			t.Errorf("GET %s: status %d, want 404", path, w.Code)
		}
	}
	wantContains(t, a.get(sam, "/nope").Body.String(), "Page not found", `href="/"`)
}

// TestServer_SlackThread_ShowsMovedNotice checks the feed badge and the
// thread page's notice for a thread moved from Slack.
func TestServer_SlackThread_ShowsMovedNotice(t *testing.T) {
	a := newApp(t)
	loc := a.createThread(sam, "Moved thread", "Body", "ai")
	plain := a.createThread(sam, "Native thread", "Body", "ai")
	a.exec(`UPDATE threads SET slack_channel_id = 'C1', slack_channel_name = 'ai-driven-development',
		slack_thread_ts = '1.2', slack_permalink = 'https://forge.slack.com/archives/C1/p12', slack_remaining = 38
		WHERE title = 'Moved thread'`)

	feed := a.get(sam, "/").Body.String()
	if n := strings.Count(feed, "moved from slack"); n != 1 {
		t.Errorf("feed shows the moved badge %d times, want 1", n)
	}
	body := a.get(sam, loc).Body.String()
	wantContains(t, body, "Moved from Slack.", "#ai-driven-development", "38 more replies stayed in the Slack thread.",
		`href="https://forge.slack.com/archives/C1/p12"`, "Open the Slack thread", "posted in Slack")
	wantNotContains(t, a.get(sam, plain).Body.String(), "Moved from Slack.", "posted in Slack")
}

// TestServer_HTMLResponses_CarrySecurityHeaders checks the headers and
// that the policy allows only the sources the pages use.
func TestServer_HTMLResponses_CarrySecurityHeaders(t *testing.T) {
	a := newApp(t)
	loc := a.createThread(sam, "Any thread", "Body", "go")
	for _, path := range []string{"/", "/threads/new", loc, "/nope"} {
		h := a.get(sam, path).Header()
		csp := h.Get("Content-Security-Policy")
		for _, want := range []string{
			"default-src 'none'", "script-src 'self'", "style-src 'self' https://fonts.googleapis.com",
			"font-src https://fonts.gstatic.com", "img-src 'self' https:", "form-action 'self'", "frame-ancestors 'none'",
		} {
			if !strings.Contains(csp, want) {
				t.Errorf("%s: CSP %q lacks %q", path, csp, want)
			}
		}
		if strings.Contains(csp, "unsafe") {
			t.Errorf("%s: CSP allows an unsafe source: %q", path, csp)
		}
		if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") != "same-origin" {
			t.Errorf("%s: headers = %v", path, h)
		}
		if ct := h.Get("Content-Type"); ct != "text/html; charset=utf-8" {
			t.Errorf("%s: Content-Type = %q", path, ct)
		}
	}
}

// TestServer_Pages_NoInlineScriptOrStyle checks the pages keep to the CSP:
// no inline script, style block, style attribute, or event handler.
func TestServer_Pages_NoInlineScriptOrStyle(t *testing.T) {
	a := newApp(t)
	loc := a.createThread(sam, "Any thread", "Body", "go")
	inline := regexp.MustCompile(`(?i)<script|<style|\sstyle=|\son[a-z]+=`)
	for _, path := range []string{"/", "/threads/new", loc, "/nope"} {
		if m := inline.FindString(a.get(sam, path).Body.String()); m != "" {
			t.Errorf("%s: page contains inline %q", path, m)
		}
	}
}

// TestServer_Assets_Served checks the embedded stylesheet, tokens, and logo.
func TestServer_Assets_Served(t *testing.T) {
	a := newApp(t)
	cases := map[string]string{
		"/assets/forum.css":      "text/css",
		"/assets/tokens.css":     "text/css",
		"/assets/forge-icon.svg": "image/svg+xml",
	}
	for path, ctype := range cases {
		w := a.get(sam, path)
		if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), ctype) || w.Body.Len() == 0 {
			t.Errorf("GET %s: status %d type %q len %d", path, w.Code, w.Header().Get("Content-Type"), w.Body.Len())
		}
	}
	wantContains(t, a.get(sam, "/assets/tokens.css").Body.String(), "--forge-flame", "--accent")
}

// TestServer_Nav_ShowsMemberAndLinks checks the nav bar: initials or
// avatar, the new-thread link, and sign out only when a portal is set.
func TestServer_Nav_ShowsMemberAndLinks(t *testing.T) {
	a := newApp(t)
	body := a.get(sam, "/").Body.String()
	wantContains(t, body, `aria-label="Signed in as Sam R"`, ">SR<", `href="/threads/new"`, "New thread", `href="/#tags"`, "forum")
	wantNotContains(t, body, "sign out")

	body = a.get(priya, "/").Body.String()
	wantContains(t, body, `src="https://avatars.example.com/priya.png"`, `alt="Signed in as Priya N"`)

	withPortal := newApp(t, func(c *config.Config) { c.AuthPortalURL = "https://auth.example.com/" })
	wantContains(t, withPortal.get(sam, "/").Body.String(), `href="https://auth.example.com/"`, "sign out")
}

// TestServer_NewThreadForm_ListsActiveTagsAndSlackNote checks the form's
// fields, that retired tags are not offered, and that the Slack note shows
// only when announcements are configured.
func TestServer_NewThreadForm_ListsActiveTagsAndSlackNote(t *testing.T) {
	a := newApp(t)
	a.exec(`UPDATE tags SET active = 0 WHERE slug = 'careers'`)
	body := a.get(sam, "/threads/new").Body.String()
	wantContains(t, body, "&gt; new thread", "Start a discussion",
		"For questions and topics that need more room than a Slack thread.",
		`<label for="title"`, `<label for="body"`, `placeholder="Ask the question or name the topic"`,
		"Markdown works. Fence code with three backticks.", "pick one or more",
		`name="tags" value="kubernetes"`, "Post thread", "Cancel", `method="post" action="/threads"`)
	wantNotContains(t, body, `value="careers"`, "Posting announces this thread in Slack")

	withSlack := newApp(t, func(c *config.Config) {
		c.SlackBotToken, c.SlackAppToken, c.SlackAnnounceChannel = "xoxb-x", "xapp-x", "C0ANNOUNCE"
	})
	wantContains(t, withSlack.get(sam, "/threads/new").Body.String(),
		"Posting announces this thread in Slack with its title, tags, and a link.")
}

// TestServer_Copy_FollowsVoiceRules checks the design system's copy rules
// on every page: no exclamation marks, no em-dashes.
func TestServer_Copy_FollowsVoiceRules(t *testing.T) {
	a := newApp(t)
	loc := a.createThread(sam, "Any thread", "Body", "go")
	tags := regexp.MustCompile(`<[^>]*>`)
	for _, path := range []string{"/", "/?tag=kubernetes", "/?page=9", "/threads/new", loc, "/nope"} {
		text := tags.ReplaceAllString(a.get(sam, path).Body.String(), " ")
		for _, bad := range []string{"!", "—"} {
			if strings.Contains(text, bad) {
				t.Errorf("%s: copy contains %q", path, bad)
			}
		}
	}
}

// TestRelTime_Buckets checks the relative time wording.
func TestRelTime_Buckets(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		ago  time.Duration
		want string
	}{
		{-time.Minute, "just now"},
		{0, "just now"},
		{59 * time.Second, "just now"},
		{time.Minute, "1m ago"},
		{59 * time.Minute, "59m ago"},
		{time.Hour, "1h ago"},
		{2*time.Hour + 30*time.Minute, "2h ago"},
		{23 * time.Hour, "23h ago"},
		{24 * time.Hour, "yesterday"},
		{47 * time.Hour, "yesterday"},
		{48 * time.Hour, "2 days ago"},
		{3*24*time.Hour + time.Hour, "3 days ago"},
		{29 * 24 * time.Hour, "29 days ago"},
		{30 * 24 * time.Hour, "Oct 18, 2026"},
		{400 * 24 * time.Hour, "Oct 13, 2025"},
	}
	for _, tc := range cases {
		if got := relTime(now, now.Add(-tc.ago)); got != tc.want {
			t.Errorf("relTime(%v ago) = %q, want %q", tc.ago, got, tc.want)
		}
	}
}

// TestInitials_FromName checks the avatar fallback.
func TestInitials_FromName(t *testing.T) {
	cases := map[string]string{
		"Sam R":             "SR",
		"priya":             "P",
		"José Núñez Ortega": "JN",
		"  dev.ortiz ":      "D",
		"":                  "?",
		"Élodie durand":     "ÉD",
	}
	for name, want := range cases {
		if got := initials(name); got != want {
			t.Errorf("initials(%q) = %q, want %q", name, got, want)
		}
	}
}

// TestServer_Thread_EditedAndVoted_Shown checks the edited mark and that
// the upvote box reflects the viewer's own vote.
func TestServer_Thread_EditedAndVoted_Shown(t *testing.T) {
	a := newApp(t)
	loc := a.createThread(sam, "Edited thread", "Body", "go")
	wantNotContains(t, a.get(priya, loc).Body.String(), "edited <time", "vote--on")

	a.exec(`UPDATE threads SET edited_at = ?, score = 1 WHERE title = 'Edited thread'`, a.store.Now().Unix())
	a.exec(`INSERT INTO votes (member_id, target_kind, target_id, created_at)
		SELECT id, 'thread', 1, 1 FROM members WHERE slack_user_id = 'U0PRIYA'`)

	body := a.get(priya, loc).Body.String()
	wantContains(t, body, "edited <time", "vote--on", `aria-label="1 upvote"`)
	wantNotContains(t, a.get(sam, loc).Body.String(), "vote--on")
	wantContains(t, a.get(priya, "/").Body.String(), "vote--on")
}
