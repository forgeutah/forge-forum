// Package realtime is the in-memory publish/subscribe hub behind the
// thread page's live updates. Subscribers are keyed by thread; an event
// published to a thread reaches every subscription on it.
//
// The hub holds no history: a subscriber hears only what is published
// while it is subscribed. It lives in one process, so it suits the
// forum's single-instance deployment and nothing larger.
package realtime

import (
	"encoding/json"
	"errors"
	"sync"
)

const (
	// MaxPerMember caps one member's subscriptions across all threads,
	// so a single member cannot exhaust the server with open tabs or a
	// script.
	MaxPerMember = 8
	// bufferSize is how many undelivered events a subscription may hold
	// before it is dropped as too slow.
	bufferSize = 16
)

// Event types.
const (
	TypeReply  = "reply"
	TypeTyping = "typing"
)

// ErrTooMany is returned by Subscribe when the member already holds
// MaxPerMember subscriptions.
var ErrTooMany = errors.New("realtime: too many subscriptions for this member")

// ErrClosed is returned by Subscribe after the hub was closed.
var ErrClosed = errors.New("realtime: hub closed")

// Event is one message to a thread's viewers. On the wire it is
//
//	{"type":"reply","reply_id":N,"parent_id":N,"member_id":N}
//	{"type":"typing","member_id":N,"name":"…"}
//
// where parent_id is 0 for a reply to the thread itself.
type Event struct {
	Type string
	// ReplyID and ParentID are set on reply events.
	ReplyID  int64
	ParentID int64
	// MemberID is who replied or is typing.
	MemberID int64
	// Name is the typing member's display name.
	Name string
}

// MarshalJSON writes only the fields the event's type carries.
func (e Event) MarshalJSON() ([]byte, error) {
	if e.Type == TypeTyping {
		return json.Marshal(struct {
			Type     string `json:"type"`
			MemberID int64  `json:"member_id"`
			Name     string `json:"name"`
		}{e.Type, e.MemberID, e.Name})
	}
	return json.Marshal(struct {
		Type     string `json:"type"`
		ReplyID  int64  `json:"reply_id"`
		ParentID int64  `json:"parent_id"`
		MemberID int64  `json:"member_id"`
	}{e.Type, e.ReplyID, e.ParentID, e.MemberID})
}

// Hub fans events out to subscriptions. It is safe for concurrent use.
type Hub struct {
	mu        sync.Mutex
	closed    bool
	threads   map[int64]map[*Subscription]struct{}
	perMember map[int64]int
}

// NewHub returns an empty hub.
func NewHub() *Hub {
	return &Hub{
		threads:   map[int64]map[*Subscription]struct{}{},
		perMember: map[int64]int{},
	}
}

// Subscription is one viewer's feed of a thread's events.
type Subscription struct {
	hub      *Hub
	threadID int64
	memberID int64
	ch       chan Event
	// done is guarded by hub.mu; it is set once ch has been closed.
	done bool
}

// Events returns the channel events arrive on. It is closed when the
// subscription ends: by Close, by Hub.Close, or because the subscriber
// fell too far behind.
func (s *Subscription) Events() <-chan Event { return s.ch }

// Close ends the subscription. It is safe to call more than once and
// after the hub dropped or closed it.
func (s *Subscription) Close() {
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	s.hub.removeLocked(s)
}

// Subscribe registers memberID as a viewer of threadID. It returns
// ErrTooMany when the member is at the cap and ErrClosed after Close.
func (h *Hub) Subscribe(threadID, memberID int64) (*Subscription, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, ErrClosed
	}
	if h.perMember[memberID] >= MaxPerMember {
		return nil, ErrTooMany
	}
	sub := &Subscription{hub: h, threadID: threadID, memberID: memberID, ch: make(chan Event, bufferSize)}
	if h.threads[threadID] == nil {
		h.threads[threadID] = map[*Subscription]struct{}{}
	}
	h.threads[threadID][sub] = struct{}{}
	h.perMember[memberID]++
	return sub, nil
}

// Publish sends ev to every subscription on threadID except those of
// exceptMemberID (0 excludes nobody). It never blocks: a subscription
// whose buffer is full is dropped and its channel closed.
func (h *Hub) Publish(threadID int64, ev Event, exceptMemberID int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.threads[threadID] {
		if exceptMemberID != 0 && sub.memberID == exceptMemberID {
			continue
		}
		select {
		case sub.ch <- ev:
		default:
			h.removeLocked(sub)
		}
	}
}

// Close ends every subscription and refuses new ones.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for _, subs := range h.threads {
		for sub := range subs {
			h.removeLocked(sub)
		}
	}
}

// removeLocked unregisters sub and closes its channel, once. Every send
// and close happens under h.mu, so no send can hit a closed channel.
func (h *Hub) removeLocked(sub *Subscription) {
	if sub.done {
		return
	}
	sub.done = true
	close(sub.ch)
	subs := h.threads[sub.threadID]
	delete(subs, sub)
	if len(subs) == 0 {
		delete(h.threads, sub.threadID)
	}
	if h.perMember[sub.memberID]--; h.perMember[sub.memberID] <= 0 {
		delete(h.perMember, sub.memberID)
	}
}
