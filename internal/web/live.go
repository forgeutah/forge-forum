package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/forgeutah/forge-forum/internal/forum"
	"github.com/forgeutah/forge-forum/internal/realtime"
)

// Live updates on the thread page. A browser with a thread open holds a
// WebSocket to GET /threads/{id}/live and hears two events (see
// realtime.Event): a reply was posted, and a member is typing. For a
// reply it then fetches GET /replies/{id}/fragment, the reply's markup
// rendered for that member, and inserts it. The only message a browser
// may send is {"type":"typing"}. assets/live.js is the client.
const (
	// liveReadLimit bounds an inbound message; the only valid one is tiny.
	liveReadLimit = 1024
	// livePingEvery keeps the connection from idling out in forge-proxy
	// or the browser; livePongWait is how long a silent peer is kept.
	livePingEvery = 25 * time.Second
	livePongWait  = 60 * time.Second
	liveWriteWait = 10 * time.Second
	// typingEvery is the most often one connection's typing is broadcast.
	typingEvery = time.Second
)

// origin is a scheme and host, as in an Origin header.
type origin struct{ scheme, host string }

// parseOrigin reads the scheme and host of an absolute http(s) URL. ok is
// false for anything else, including the "null" origin.
func parseOrigin(raw string) (origin, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return origin{}, false
	}
	return origin{scheme: u.Scheme, host: strings.ToLower(u.Host)}, true
}

// connectSrc is the Content-Security-Policy directive that lets live.js
// open its socket on the public host. 'self' alone is not enough: some
// browsers do not match it against wss:.
func (o origin) connectSrc() string {
	switch o.scheme {
	case "https":
		return "connect-src 'self' wss://" + o.host
	case "http": // local use
		return "connect-src 'self' wss://" + o.host + " ws://" + o.host
	}
	return "connect-src 'self'"
}

// sameOrigin reports whether the request's Origin header names the
// forum's public address. forge-proxy authenticates the handshake from
// the browser's session cookie, which the browser sends no matter which
// site opened the socket, so this check is what stops another site
// reading a member's events (cross-site WebSocket hijacking). The Host
// header is no help: behind forge-proxy it is the upstream's own name.
func (s *Server) sameOrigin(r *http.Request) bool {
	got, ok := parseOrigin(r.Header.Get("Origin"))
	return ok && s.origin.host != "" && got == s.origin
}

// Close ends every live connection and refuses new ones; pages and forms
// keep working. Call it when shutting down, before or alongside
// http.Server.Shutdown, which does not wait for or close WebSockets.
func (s *Server) Close() { s.hub.Close() }

// publishReply tells a thread's viewers about a new reply. It is the
// store's reply listener, so it runs after the reply has committed. The
// event goes to the author's own connections too: their other tabs want
// it, and the page they land on already holds the reply and skips it.
// Receiving it also clears the author's typing indicator for everyone.
func (s *Server) publishReply(threadID, replyID, parentID, authorID int64) {
	s.hub.Publish(threadID, realtime.Event{
		Type: realtime.TypeReply, ReplyID: replyID, ParentID: parentID, MemberID: authorID,
	}, 0)
}

// handleLive upgrades to a WebSocket and relays the thread's events until
// either side closes.
func (s *Server) handleLive(w http.ResponseWriter, r *http.Request) {
	m := member(r)
	threadID, ok := pathID(r)
	if !ok {
		s.handleNotFound(w, r)
		return
	}
	t, err := s.store.GetThread(r.Context(), threadID, m.ID)
	if errors.Is(err, forum.ErrNotFound) {
		s.handleNotFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if t.DeletedAt != nil {
		http.Error(w, "This thread was removed.", http.StatusGone)
		return
	}
	if !s.sameOrigin(r) {
		s.logger.Warn("live connection refused", "reason", "origin", "origin", r.Header.Get("Origin"), "member_id", m.ID)
		http.Error(w, "Live updates are only available from the forum itself.", http.StatusForbidden)
		return
	}

	// Subscribe before upgrading, so the cap can still answer over HTTP
	// and no event is missed between the handshake and the first read.
	sub, err := s.hub.Subscribe(threadID, m.ID)
	switch {
	case errors.Is(err, realtime.ErrTooMany):
		http.Error(w, "Too many live connections. Close a few forum tabs.", http.StatusTooManyRequests)
		return
	case err != nil: // the hub is closed: the server is shutting down
		http.Error(w, "Live updates are unavailable right now.", http.StatusServiceUnavailable)
		return
	}
	defer sub.Close()

	upgrader := websocket.Upgrader{
		HandshakeTimeout: liveWriteWait,
		ReadBufferSize:   liveReadLimit,
		WriteBufferSize:  1024,
		CheckOrigin:      s.sameOrigin,
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade has already answered
	}
	s.logger.Debug("live connected", "member_id", m.ID, "thread_id", threadID)

	// One goroutine writes; this one reads. When either stops it ends the
	// other: the writer closes the connection, which fails the read, and
	// the reader closes the subscription, which stops the writer.
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer conn.Close()
		liveWrite(conn, sub)
	}()

	conn.SetReadLimit(liveReadLimit)
	conn.SetReadDeadline(time.Now().Add(livePongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(livePongWait))
	})
	typing := realtime.Event{Type: realtime.TypeTyping, MemberID: m.ID, Name: m.Name}
	var lastTyping time.Time
	for {
		kind, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		// Only "typing" is accepted, and only the fact of it: who is
		// typing comes from the authenticated member, never the message.
		var msg struct {
			Type string `json:"type"`
		}
		if kind != websocket.TextMessage || json.Unmarshal(data, &msg) != nil || msg.Type != realtime.TypeTyping {
			continue
		}
		if now := time.Now(); now.Sub(lastTyping) >= typingEvery {
			lastTyping = now
			s.hub.Publish(threadID, typing, m.ID)
		}
	}
	sub.Close()
	<-done
	s.logger.Debug("live disconnected", "member_id", m.ID, "thread_id", threadID)
}

// liveWrite sends the subscription's events and regular pings until the
// subscription ends or a write fails.
func liveWrite(conn *websocket.Conn, sub *realtime.Subscription) {
	ping := time.NewTicker(livePingEvery)
	defer ping.Stop()
	for {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				// Closed by the reader, by shutdown, or for falling behind.
				conn.WriteControl(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseGoingAway, ""), time.Now().Add(liveWriteWait))
				return
			}
			conn.SetWriteDeadline(time.Now().Add(liveWriteWait))
			if err := conn.WriteJSON(ev); err != nil {
				return
			}
		case <-ping.C:
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(liveWriteWait)); err != nil {
				return
			}
		}
	}
}

// handleReplyFragment returns one reply's markup, without its children,
// rendered for the requesting member exactly as the thread page would:
// their vote, their controls, their CSRF token. live.js inserts it when a
// reply event arrives.
func (s *Server) handleReplyFragment(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		s.handleNotFound(w, r)
		return
	}
	m := member(r)
	reply, err := s.store.GetReply(r.Context(), id)
	if err != nil {
		s.refuse(w, r, err)
		return
	}
	t, err := s.store.GetThread(r.Context(), reply.ThreadID, m.ID)
	if err != nil {
		s.refuse(w, r, err)
		return
	}
	tree, _, err := s.store.ReplyTree(r.Context(), reply.ThreadID, m.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	node, parent := findReply(tree, nil, id)
	if node == nil || node.Deleted {
		// Deleted between the two reads.
		s.refuse(w, r, forum.ErrDeleted)
		return
	}
	alone := *node
	alone.Children = nil
	placed := false
	views := replyViews([]*forum.ReplyNode{&alone}, parent, t, m, s.csrfToken(m), replyDraft{}, &placed)

	name := "reply"
	if parent == nil {
		name = "reply-card" // a top-level reply comes in its card
	}
	var buf bytes.Buffer
	if err := s.pages["thread"].ExecuteTemplate(&buf, name, views[0]); err != nil {
		s.serverError(w, r, fmt.Errorf("render reply fragment: %w", err))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(buf.Bytes())
}

// findReply returns the node with the given ID and its parent, which is
// nil at the top level. Both are nil when the tree does not hold it.
func findReply(nodes []*forum.ReplyNode, parent *forum.ReplyNode, id int64) (node, itsParent *forum.ReplyNode) {
	for _, n := range nodes {
		if n.ID == id {
			return n, parent
		}
		if found, p := findReply(n.Children, n, id); found != nil {
			return found, p
		}
	}
	return nil, nil
}
