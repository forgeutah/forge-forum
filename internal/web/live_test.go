package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/forgeutah/forge-forum/internal/config"
	"github.com/forgeutah/forge-forum/internal/realtime"
)

const (
	testOrigin = "https://forum.example.com" // newApp's PublicBaseURL
	liveWait   = 5 * time.Second
)

// liveEvent is an event as a browser would decode it.
type liveEvent struct {
	Type     string `json:"type"`
	ReplyID  int64  `json:"reply_id"`
	ParentID int64  `json:"parent_id"`
	MemberID int64  `json:"member_id"`
	Name     string `json:"name"`
}

// serve puts the app's real handler chain on a listening server, so a
// WebSocket client can connect to it.
func (a *app) serve() *httptest.Server {
	a.t.Helper()
	ts := httptest.NewServer(a.h)
	a.t.Cleanup(func() {
		a.srv.Close() // ends live connections so the server can stop
		ts.Close()
	})
	return ts
}

// memberID returns the forum's ID for u, signing them in first.
func (a *app) memberID(u user) int64 {
	a.t.Helper()
	a.token(u)
	return int64(a.count(`SELECT id FROM members WHERE slack_user_id = ?`, u.slackID))
}

// liveHeader is what forge-proxy and a browser on the forum's own page
// would send with the WebSocket handshake.
func liveHeader(u user, origin string) http.Header {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	u.sign(r)
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	return r.Header
}

func dialLive(ts *httptest.Server, path string, h http.Header) (*websocket.Conn, *http.Response, error) {
	d := websocket.Dialer{HandshakeTimeout: liveWait}
	return d.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+path, h)
}

// wantRefused checks a handshake is turned away with status and no upgrade.
func wantRefused(t *testing.T, ts *httptest.Server, path string, h http.Header, status int) {
	t.Helper()
	c, resp, err := dialLive(ts, path, h)
	if err == nil {
		c.Close()
		t.Errorf("%s: handshake succeeded, want status %d", path, status)
		return
	}
	if resp == nil {
		t.Errorf("%s: %v, want status %d", path, err, status)
		return
	}
	if resp.StatusCode != status || resp.Header.Get("Upgrade") != "" {
		t.Errorf("%s: status %d (Upgrade %q), want %d and no upgrade", path, resp.StatusCode, resp.Header.Get("Upgrade"), status)
	}
}

// liveConn is one browser's live connection. A goroutine reads it, so
// tests wait on channels with a timeout and never sleep.
type liveConn struct {
	t      *testing.T
	c      *websocket.Conn
	events chan liveEvent // closed when the socket ends
	pongs  chan struct{}
}

// live connects u to a thread's live endpoint; the handshake must succeed.
func (a *app) live(ts *httptest.Server, u user, thread string) *liveConn {
	a.t.Helper()
	c, resp, err := dialLive(ts, thread+"/live", liveHeader(u, testOrigin))
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		a.t.Fatalf("connect %s as %s: %v (status %d)", thread, u.name, err, status)
	}
	l := &liveConn{t: a.t, c: c, events: make(chan liveEvent, 64), pongs: make(chan struct{}, 8)}
	c.SetPongHandler(func(string) error {
		l.pongs <- struct{}{}
		return nil
	})
	a.t.Cleanup(func() { c.Close() })
	go func() {
		defer close(l.events)
		for {
			_, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			var ev liveEvent
			if err := json.Unmarshal(data, &ev); err != nil {
				ev = liveEvent{Type: "undecodable: " + string(data)}
			}
			l.events <- ev
		}
	}()
	return l
}

// next returns the connection's next event.
func (l *liveConn) next() liveEvent {
	l.t.Helper()
	select {
	case ev, ok := <-l.events:
		if !ok {
			l.t.Fatal("live connection ended, want an event")
		}
		return ev
	case <-time.After(liveWait):
		l.t.Fatal("no live event arrived")
	}
	return liveEvent{}
}

func (l *liveConn) want(want liveEvent) {
	l.t.Helper()
	if got := l.next(); got != want {
		l.t.Errorf("live event = %+v, want %+v", got, want)
	}
}

// wantEnded waits for the server to end the connection.
func (l *liveConn) wantEnded() {
	l.t.Helper()
	deadline := time.After(liveWait)
	for {
		select {
		case ev, ok := <-l.events:
			if !ok {
				return
			}
			l.t.Errorf("got event %+v, want the connection to end", ev)
		case <-deadline:
			l.t.Fatal("live connection still open")
		}
	}
}

func (l *liveConn) send(kind int, msg string) {
	l.t.Helper()
	if err := l.c.WriteMessage(kind, []byte(msg)); err != nil {
		l.t.Fatalf("send %q: %v", msg, err)
	}
}

// sync returns once the server has read everything sent so far: frames
// are read in order, and the server answers a ping when it reads it.
func (l *liveConn) sync() {
	l.t.Helper()
	if err := l.c.WriteControl(websocket.PingMessage, nil, time.Now().Add(liveWait)); err != nil {
		l.t.Fatalf("ping: %v", err)
	}
	select {
	case <-l.pongs:
	case <-time.After(liveWait):
		l.t.Fatal("no pong from the server")
	}
}

const typingMsg = `{"type":"typing"}`

// TestLive_Reply_ReachesViewersOfThatThreadOnly posts replies through the
// normal form and checks who hears about them, and with which IDs.
func TestLive_Reply_ReachesViewersOfThatThreadOnly(t *testing.T) {
	a := newApp(t)
	th := a.createThread(sam, "Live thread", "Body", "go")
	elsewhere := a.createThread(sam, "Another thread", "Body", "go")
	ts := a.serve()
	samID, priyaID := a.memberID(sam), a.memberID(priya)

	viewer := a.live(ts, dana, th)
	away := a.live(ts, lee, elsewhere)

	top := a.reply(sam, th, 0, "Top level from Sam")
	viewer.want(liveEvent{Type: "reply", ReplyID: top, ParentID: 0, MemberID: samID})
	nested := a.reply(priya, th, top, "Nested from Priya")
	viewer.want(liveEvent{Type: "reply", ReplyID: nested, ParentID: top, MemberID: priyaID})

	// The viewer of the other thread heard none of that: the first thing
	// they hear is the reply on their own thread.
	theirs := a.reply(sam, elsewhere, 0, "Over here")
	away.want(liveEvent{Type: "reply", ReplyID: theirs, ParentID: 0, MemberID: samID})

	// A refused reply is not announced; the next event is the next real one.
	if w := a.submit(sam, th+"/replies", "body", "  "); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("empty reply: status %d", w.Code)
	}
	after := a.reply(sam, th, 0, "After the refusal")
	viewer.want(liveEvent{Type: "reply", ReplyID: after, ParentID: 0, MemberID: samID})
}

// TestLive_Fragment_OneReplyForTheRequestingMember checks the markup a
// browser fetches after a reply event.
func TestLive_Fragment_OneReplyForTheRequestingMember(t *testing.T) {
	a := newApp(t)
	th := a.createThread(sam, "Live thread", "Body", "go")
	top := a.reply(sam, th, 0, "Top level **from Sam** <script>alert(1)</script>")
	nested := a.reply(priya, th, top, "Nested from Priya")
	deep := nested
	for i := range maxIndentLevels {
		deep = a.reply(dana, th, deep, fmt.Sprintf("Deeper %d", i))
	}
	a.mustSubmit(dana, replyPath(top)+"/vote", "vote", "on")

	fragment := func(u user, id int64) string {
		t.Helper()
		w := a.get(u, replyPath(id)+"/fragment")
		if w.Code != http.StatusOK {
			t.Fatalf("GET fragment %d as %s: status %d", id, u.name, w.Code)
		}
		if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("fragment Cache-Control = %q, want no-store", cc)
		}
		if ct := w.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
			t.Errorf("fragment Content-Type = %q", ct)
		}
		return w.Body.String()
	}

	// A top-level reply comes in its card, rendered for Dana: her token,
	// her vote, no edit link, the same sanitised Markdown as the page.
	body := fragment(dana, top)
	if !strings.HasPrefix(strings.TrimSpace(body), `<section class="reply-card"`) {
		t.Errorf("top-level fragment does not start with its card: %.80q", body)
	}
	wantContains(t, body, anchor(top), "<strong>from Sam</strong>", "@Sam R",
		`value="`+a.token(dana)+`"`, `aria-pressed="true"`, `name="parent" value="`+fmt.Sprint(top)+`"`)
	wantNotContains(t, body, a.token(sam), replyPath(top)+"/edit", "<script",
		"<html", "<nav", "reply-children", anchor(nested), "Nested from Priya")

	// The same reply for its author has their controls instead.
	mine := fragment(sam, top)
	wantContains(t, mine, `value="`+a.token(sam)+`"`, replyPath(top)+"/edit", `aria-pressed="false"`)
	wantNotContains(t, mine, a.token(dana))

	// A nested reply comes bare, to go inside its parent's children.
	body = fragment(dana, nested)
	if !strings.HasPrefix(strings.TrimSpace(body), `<div class="reply"`) {
		t.Errorf("nested fragment does not start with the reply: %.80q", body)
	}
	wantContains(t, body, anchor(nested), "Nested from Priya")
	wantNotContains(t, body, "reply-card", "reply-children", "Deeper 0")

	// Past the indent limit the fragment says who it answers, as the page does.
	wantContains(t, fragment(sam, deep), "replying to @Dana O", "data-flat-children")
	if page := a.page(sam, th); !strings.Contains(page, "replying to @Dana O") {
		t.Error("the thread page lost its replying-to note")
	}
}

func TestLive_Fragment_MissingOrDeleted_Refused(t *testing.T) {
	a := newApp(t)
	th := a.createThread(sam, "Live thread", "Body", "go")
	gone := a.reply(sam, th, 0, "Soon gone")
	a.reply(dana, th, gone, "Under the deleted reply")
	a.mustSubmit(sam, replyPath(gone)+"/delete")
	dead := a.createThread(sam, "Dead thread", "Body", "go")
	orphan := a.reply(dana, dead, 0, "In a removed thread")
	a.mustSubmit(sam, dead+"/delete")

	cases := map[string]int{
		replyPath(gone) + "/fragment":   http.StatusGone,
		replyPath(orphan) + "/fragment": http.StatusGone,
		"/replies/999/fragment":         http.StatusNotFound,
		"/replies/abc/fragment":         http.StatusNotFound,
	}
	for path, want := range cases {
		w := a.get(dana, path)
		if w.Code != want {
			t.Errorf("GET %s: status %d, want %d", path, w.Code, want)
		}
		wantNotContains(t, w.Body.String(), "Soon gone", "In a removed thread")
	}
	if w := a.do(httptest.NewRequest(http.MethodGet, replyPath(gone)+"/fragment", nil)); w.Code != http.StatusUnauthorized {
		t.Errorf("fragment without the proxy contract: status %d, want 401", w.Code)
	}
}

// TestLive_Typing_ReachesOthersWithServerStampedName checks typing goes
// to the thread's other members and never back to the sender.
func TestLive_Typing_ReachesOthersWithServerStampedName(t *testing.T) {
	a := newApp(t)
	th := a.createThread(sam, "Live thread", "Body", "go")
	elsewhere := a.createThread(sam, "Another thread", "Body", "go")
	ts := a.serve()
	samID := a.memberID(sam)

	typist := a.live(ts, sam, th)
	sameMember := a.live(ts, sam, th) // Sam's second tab
	viewer := a.live(ts, dana, th)
	away := a.live(ts, lee, elsewhere)

	typist.send(websocket.TextMessage, typingMsg)
	viewer.want(liveEvent{Type: "typing", MemberID: samID, Name: "Sam R"})

	// Events arrive in publish order, so a reply landing first on Sam's
	// own connections and on the other thread's proves the typing event
	// never went there.
	id := a.reply(sam, th, 0, "Done typing")
	reply := liveEvent{Type: "reply", ReplyID: id, MemberID: samID}
	typist.want(reply)
	sameMember.want(reply)
	viewer.want(reply)
	other := a.reply(sam, elsewhere, 0, "Over here")
	away.want(liveEvent{Type: "reply", ReplyID: other, MemberID: samID})
}

// TestLive_Typing_ForgedFieldsAndOtherMessages_Ignored checks the server
// takes nothing from an inbound message but the fact of typing.
func TestLive_Typing_ForgedFieldsAndOtherMessages_Ignored(t *testing.T) {
	a := newApp(t)
	th := a.createThread(sam, "Live thread", "Body", "go")
	ts := a.serve()
	samID := a.memberID(sam)

	forger := a.live(ts, sam, th)
	viewer := a.live(ts, dana, th)

	for _, junk := range []string{
		`{"type":"reply","reply_id":1,"parent_id":0,"member_id":1}`,
		`{"type":"TYPING"}`, `{"type":7}`, `"typing"`, `not json`, `[]`, ``,
	} {
		forger.send(websocket.TextMessage, junk)
	}
	forger.send(websocket.BinaryMessage, typingMsg)
	forger.send(websocket.TextMessage, `{"type":"typing","member_id":9999,"name":"Mallory <b>admin</b>","reply_id":5}`)

	viewer.want(liveEvent{Type: "typing", MemberID: samID, Name: "Sam R"})
}

func TestLive_Typing_RateLimited(t *testing.T) {
	a := newApp(t)
	th := a.createThread(sam, "Live thread", "Body", "go")
	ts := a.serve()
	samID := a.memberID(sam)

	typist := a.live(ts, sam, th)
	viewer := a.live(ts, dana, th)

	for range 5 {
		typist.send(websocket.TextMessage, typingMsg)
	}
	typist.sync() // the server has now read all five
	id := a.reply(sam, th, 0, "Done typing")

	viewer.want(liveEvent{Type: "typing", MemberID: samID, Name: "Sam R"})
	viewer.want(liveEvent{Type: "reply", ReplyID: id, MemberID: samID})
}

// TestLive_Handshake_Refused covers who may not open a live connection.
// The Origin check is the defence against another site opening the
// socket with the member's proxy session.
func TestLive_Handshake_Refused(t *testing.T) {
	a := newApp(t)
	th := a.createThread(sam, "Live thread", "Body", "go")
	dead := a.createThread(sam, "Dead thread", "Body", "go")
	a.mustSubmit(sam, dead+"/delete")
	ts := a.serve()

	for _, origin := range []string{
		"", "https://evil.example.com", "http://forum.example.com", "https://forum.example.com.evil.test",
		"https://forum.example.com:8443", "null", "forum.example.com", ts.URL,
	} {
		t.Run("origin "+origin, func(t *testing.T) {
			wantRefused(t, ts, th+"/live", liveHeader(sam, origin), http.StatusForbidden)
		})
	}

	noContract := http.Header{"Origin": {testOrigin}}
	wantRefused(t, ts, th+"/live", noContract, http.StatusUnauthorized)
	wantRefused(t, ts, "/threads/999/live", liveHeader(sam, testOrigin), http.StatusNotFound)
	wantRefused(t, ts, "/threads/abc/live", liveHeader(sam, testOrigin), http.StatusNotFound)
	wantRefused(t, ts, dead+"/live", liveHeader(sam, testOrigin), http.StatusGone)

	// A plain GET with the right Origin is not a handshake.
	r := httptest.NewRequest(http.MethodGet, th+"/live", nil)
	sam.sign(r)
	r.Header.Set("Origin", testOrigin)
	if w := a.do(r); w.Code != http.StatusBadRequest {
		t.Errorf("plain GET of the live endpoint: status %d, want 400", w.Code)
	}

	// Nothing above left a subscription behind.
	for range realtime.MaxPerMember {
		a.live(ts, sam, th)
	}
}

func TestLive_Handshake_PerMemberCap(t *testing.T) {
	a := newApp(t)
	th := a.createThread(sam, "Live thread", "Body", "go")
	ts := a.serve()

	conns := make([]*liveConn, 0, realtime.MaxPerMember)
	for range realtime.MaxPerMember {
		conns = append(conns, a.live(ts, sam, th))
	}
	wantRefused(t, ts, th+"/live", liveHeader(sam, testOrigin), http.StatusTooManyRequests)
	a.live(ts, dana, th) // another member is unaffected

	// Closing a tab frees its slot once the server notices.
	conns[0].c.Close()
	deadline := time.Now().Add(liveWait)
	for {
		c, _, err := dialLive(ts, th+"/live", liveHeader(sam, testOrigin))
		if err == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("slot never freed after a connection closed: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestLive_OversizedMessage_EndsConnection(t *testing.T) {
	a := newApp(t)
	th := a.createThread(sam, "Live thread", "Body", "go")
	ts := a.serve()

	l := a.live(ts, sam, th)
	l.send(websocket.TextMessage, `{"type":"typing","pad":"`+strings.Repeat("x", 2048)+`"}`)
	l.wantEnded()
}

func TestServer_Close_EndsLiveConnections(t *testing.T) {
	a := newApp(t)
	th := a.createThread(sam, "Live thread", "Body", "go")
	ts := a.serve()

	one, two := a.live(ts, sam, th), a.live(ts, dana, th)
	a.srv.Close()
	one.wantEnded()
	two.wantEnded()

	wantRefused(t, ts, th+"/live", liveHeader(sam, testOrigin), http.StatusServiceUnavailable)
	// The pages and the reply form still work while the process drains.
	a.reply(sam, th, 0, "Posted during shutdown")
	a.srv.Close() // twice is fine
}

// TestLive_ThreadPage_CarriesHooksAndNoInlineScript checks what live.js
// reads from the page, and that the page still keeps to the CSP.
func TestLive_ThreadPage_CarriesHooksAndNoInlineScript(t *testing.T) {
	a := newApp(t)
	th := a.createThread(sam, "Live thread", "Body", "go")
	id := strings.TrimPrefix(th, "/threads/")
	danaID := a.memberID(dana)

	empty := a.page(dana, th)
	wantContains(t, empty,
		liveScriptTag,
		`data-thread-id="`+id+`"`,
		fmt.Sprintf(`data-member-id="%d"`, danaID),
		`data-live-path="`+th+`/live"`,
		`<div class="reply-list" id="reply-list"></div>`,
		`id="reply-count" data-count="0"`,
		`<p class="typing" id="typing-indicator" hidden></p>`,
		`<p class="visually-hidden" id="live-announcer" aria-live="polite"></p>`,
	)
	if strings.Count(empty, "<script") != 1 {
		t.Errorf("thread page has %d script tags, want 1", strings.Count(empty, "<script"))
	}
	if m := findInline(empty); m != "" {
		t.Errorf("thread page contains inline %q", m)
	}

	top := a.reply(sam, th, 0, "Top")
	a.reply(dana, th, top, "Nested")
	full := a.page(dana, th)
	wantContains(t, full, `id="reply-count" data-count="2"`, "2 replies")
	wantOrder(t, full, `id="reply-list"`, `<section class="reply-card"`, anchor(top), `class="reply-children"`, "Nested", `id="typing-indicator"`, `id="reply-body"`)

	// Only the thread page loads the script.
	for _, path := range []string{"/", "/threads/new", "/settings", th + "/edit"} {
		wantNotContains(t, a.get(sam, path).Body.String(), "<script")
	}

	js := a.get(sam, "/assets/live.js")
	if js.Code != http.StatusOK || !strings.HasPrefix(js.Header().Get("Content-Type"), "text/javascript") {
		t.Errorf("GET /assets/live.js: status %d type %q", js.Code, js.Header().Get("Content-Type"))
	}
}

// TestLive_CSP_AllowsTheSocketOnThePublicHostOnly checks connect-src is
// derived from the public address.
func TestLive_CSP_AllowsTheSocketOnThePublicHostOnly(t *testing.T) {
	cases := map[string]struct{ want, bad string }{
		"https://forum.example.com": {"connect-src 'self' wss://forum.example.com;", "ws://"},
		"http://localhost:8080":     {"connect-src 'self' wss://localhost:8080 ws://localhost:8080;", "*"},
	}
	for base, tc := range cases {
		a := newApp(t, func(c *config.Config) { c.PublicBaseURL = base })
		th := a.createThread(sam, "Live thread", "Body", "go")
		for _, path := range []string{th, "/"} {
			csp := a.get(sam, path).Header().Get("Content-Security-Policy")
			if !strings.Contains(csp+";", tc.want) {
				t.Errorf("%s %s: CSP %q lacks %q", base, path, csp, tc.want)
			}
			if strings.Contains(csp, tc.bad) || strings.Contains(csp, "unsafe") {
				t.Errorf("%s %s: CSP %q is too wide", base, path, csp)
			}
			if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "script-src 'self'") {
				t.Errorf("%s %s: CSP %q lost its base policy", base, path, csp)
			}
		}
	}
}
