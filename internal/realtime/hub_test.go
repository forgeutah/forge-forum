package realtime

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

func mustSubscribe(t *testing.T, h *Hub, thread, member int64) *Subscription {
	t.Helper()
	sub, err := h.Subscribe(thread, member)
	if err != nil {
		t.Fatalf("Subscribe(%d, %d): %v", thread, member, err)
	}
	return sub
}

// next returns the subscription's next event, failing if none is waiting.
func next(t *testing.T, sub *Subscription) Event {
	t.Helper()
	select {
	case ev, ok := <-sub.Events():
		if !ok {
			t.Fatal("subscription closed, want an event")
		}
		return ev
	default:
		t.Fatal("no event waiting")
		return Event{}
	}
}

func wantIdle(t *testing.T, sub *Subscription) {
	t.Helper()
	select {
	case ev, ok := <-sub.Events():
		t.Fatalf("got event %+v (open %v), want nothing", ev, ok)
	default:
	}
}

func wantClosed(t *testing.T, sub *Subscription) {
	t.Helper()
	for {
		select {
		case _, ok := <-sub.Events():
			if !ok {
				return
			}
		default:
			t.Fatal("subscription still open, want closed")
		}
	}
}

func TestHub_Publish_ReachesOnlyThatThread(t *testing.T) {
	h := NewHub()
	a := mustSubscribe(t, h, 1, 10)
	b := mustSubscribe(t, h, 1, 11)
	other := mustSubscribe(t, h, 2, 12)

	ev := Event{Type: TypeReply, ReplyID: 7, ParentID: 3, MemberID: 99}
	h.Publish(1, ev, 0)

	if got := next(t, a); got != ev {
		t.Errorf("a got %+v, want %+v", got, ev)
	}
	if got := next(t, b); got != ev {
		t.Errorf("b got %+v, want %+v", got, ev)
	}
	wantIdle(t, other)
}

func TestHub_Publish_SkipsExceptMember(t *testing.T) {
	h := NewHub()
	mine := mustSubscribe(t, h, 1, 10)
	mineToo := mustSubscribe(t, h, 1, 10)
	theirs := mustSubscribe(t, h, 1, 11)

	h.Publish(1, Event{Type: TypeTyping, MemberID: 10, Name: "Ada"}, 10)

	wantIdle(t, mine)
	wantIdle(t, mineToo)
	if got := next(t, theirs); got.Name != "Ada" {
		t.Errorf("got %+v", got)
	}
}

func TestHub_Publish_FullBuffer_DropsSubscriberWithoutBlocking(t *testing.T) {
	h := NewHub()
	slow := mustSubscribe(t, h, 1, 10)
	healthy := mustSubscribe(t, h, 1, 11)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range bufferSize + 1 {
			h.Publish(1, Event{Type: TypeReply, ReplyID: int64(i + 1)}, 0)
			<-healthy.Events() // healthy keeps up
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked on a full subscriber")
	}

	wantClosed(t, slow)
	// The dropped subscriber no longer counts against its member's cap,
	// and the healthy one still hears events.
	h.Publish(1, Event{Type: TypeReply, ReplyID: 1000}, 0)
	if got := next(t, healthy); got.ReplyID != 1000 {
		t.Errorf("healthy got %+v", got)
	}
	for range MaxPerMember {
		mustSubscribe(t, h, 5, 10)
	}
	slow.Close() // closing a dropped subscription is harmless
}

func TestHub_Subscribe_PerMemberCap(t *testing.T) {
	h := NewHub()
	subs := make([]*Subscription, 0, MaxPerMember)
	for i := range MaxPerMember {
		subs = append(subs, mustSubscribe(t, h, int64(i+1), 10))
	}
	if _, err := h.Subscribe(1, 10); !errors.Is(err, ErrTooMany) {
		t.Fatalf("subscription over the cap: err = %v, want ErrTooMany", err)
	}
	// Another member is unaffected, and closing one frees a slot.
	mustSubscribe(t, h, 1, 11)
	subs[0].Close()
	subs[0].Close()
	mustSubscribe(t, h, 1, 10)
	if _, err := h.Subscribe(1, 10); !errors.Is(err, ErrTooMany) {
		t.Fatalf("double Close freed two slots: err = %v", err)
	}
}

func TestHub_Close_EndsSubscriptions(t *testing.T) {
	h := NewHub()
	a := mustSubscribe(t, h, 1, 10)
	b := mustSubscribe(t, h, 2, 11)
	h.Close()
	h.Close()

	wantClosed(t, a)
	wantClosed(t, b)
	if _, err := h.Subscribe(1, 10); !errors.Is(err, ErrClosed) {
		t.Errorf("Subscribe after Close: err = %v, want ErrClosed", err)
	}
	h.Publish(1, Event{Type: TypeReply}, 0) // must not panic
	a.Close()
}

func TestHub_ConcurrentUse(t *testing.T) {
	h := NewHub()
	var wg sync.WaitGroup
	for m := range 8 {
		wg.Go(func() {
			for i := range 200 {
				sub, err := h.Subscribe(int64(i%3), int64(m+1))
				if err != nil {
					continue
				}
				h.Publish(int64(i%3), Event{Type: TypeTyping, MemberID: int64(m + 1)}, 0)
				select {
				case <-sub.Events():
				default:
				}
				sub.Close()
			}
		})
	}
	wg.Go(func() {
		for i := range 500 {
			h.Publish(int64(i%3), Event{Type: TypeReply, ReplyID: int64(i)}, 0)
		}
	})
	wg.Wait()
	h.Close()
}

func TestEvent_JSON(t *testing.T) {
	cases := map[string]Event{
		`{"type":"reply","reply_id":7,"parent_id":0,"member_id":4}`: {Type: TypeReply, ReplyID: 7, MemberID: 4},
		`{"type":"reply","reply_id":8,"parent_id":7,"member_id":4}`: {Type: TypeReply, ReplyID: 8, ParentID: 7, MemberID: 4},
		`{"type":"typing","member_id":4,"name":"Ada L"}`:            {Type: TypeTyping, MemberID: 4, Name: "Ada L"},
	}
	for want, ev := range cases {
		got, err := json.Marshal(ev)
		if err != nil || string(got) != want {
			t.Errorf("Marshal(%+v) = %s, %v\nwant %s", ev, got, err, want)
		}
	}
}
