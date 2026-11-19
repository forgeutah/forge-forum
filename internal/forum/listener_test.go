package forum

import (
	"context"
	"errors"
	"testing"
)

// heard is one call to the reply listener.
type heard struct{ thread, reply, parent, author int64 }

// TestStore_ReplyListener_FiresOnceAfterCommit checks the listener hears
// each stored reply once, with its IDs, after the reply is readable.
func TestStore_ReplyListener_FiresOnceAfterCommit(t *testing.T) {
	s, d := newTestStore(t)
	sam := addMember(t, d, "U0SAM", "Sam")
	dana := addMember(t, d, "U0DANA", "Dana")
	th := mustCreate(t, s, sam, "Thread", "go")

	var calls []heard
	s.SetReplyListener(func(threadID, replyID, parentID, authorID int64) {
		// The transaction has committed: the reply is visible to readers.
		if _, err := s.GetReply(context.Background(), replyID); err != nil {
			t.Errorf("listener ran before reply %d was readable: %v", replyID, err)
		}
		calls = append(calls, heard{threadID, replyID, parentID, authorID})
	})

	top := replyAs(t, s, dana, th, 0, "top")
	nested := replyAs(t, s, sam, th, top, "nested")

	want := []heard{{th, top, 0, dana}, {th, nested, top, sam}}
	if len(calls) != len(want) {
		t.Fatalf("listener calls = %+v, want %+v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Errorf("call %d = %+v, want %+v", i, calls[i], want[i])
		}
	}
}

// TestStore_ReplyListener_SilentOnRefusedReply checks a reply that was
// refused, and so rolled back, is never announced.
func TestStore_ReplyListener_SilentOnRefusedReply(t *testing.T) {
	s, d := newTestStore(t)
	ctx := context.Background()
	sam := addMember(t, d, "U0SAM", "Sam")
	th := mustCreate(t, s, sam, "Thread", "go")
	dead := mustCreate(t, s, sam, "Dead", "go")
	if _, err := s.DeletePost(ctx, Actor{ID: sam}, threadRef(dead)); err != nil {
		t.Fatal(err)
	}

	calls := 0
	s.SetReplyListener(func(_, _, _, _ int64) { calls++ })

	var invalid ValidationError
	if _, err := s.CreateReply(ctx, sam, th, 0, "  "); !errors.As(err, &invalid) {
		t.Errorf("empty body: err = %v", err)
	}
	if _, err := s.CreateReply(ctx, sam, dead, 0, "hello"); !errors.Is(err, ErrDeleted) {
		t.Errorf("deleted thread: err = %v", err)
	}
	if _, err := s.CreateReply(ctx, sam, th, 999, "hello"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing parent: err = %v", err)
	}
	// A reply whose transaction fails on a database error, not a refusal:
	// its author has no member row.
	s.SetNotifier(Notifier{BaseURL: "https://forum.test"})
	before := countRows(t, d, "replies")
	if _, err := s.CreateReply(ctx, sam+1000, th, 0, "hello"); err == nil {
		t.Error("reply by a missing member: want an error")
	}
	if n := countRows(t, d, "replies"); n != before {
		t.Errorf("replies = %d, want %d: the failed reply was stored", n, before)
	}
	if calls != 0 {
		t.Errorf("listener calls = %d, want 0", calls)
	}
}

// TestStore_ReplyListener_SilentOnImport checks replies moved from Slack
// are not announced.
func TestStore_ReplyListener_SilentOnImport(t *testing.T) {
	s, d := newImportStore(t)
	calls := 0
	s.SetReplyListener(func(_, _, _, _ int64) { calls++ })

	if _, _, err := s.ImportSlackThread(context.Background(), importInput()); err != nil {
		t.Fatalf("ImportSlackThread: %v", err)
	}
	if n := importCount(t, d, "replies"); n == 0 {
		t.Fatal("the import stored no replies; the test proves nothing")
	}
	if calls != 0 {
		t.Errorf("listener calls = %d, want 0", calls)
	}
}

// TestStore_ReplyListener_NilIsNoOp checks replies work with no listener
// and after one is cleared.
func TestStore_ReplyListener_NilIsNoOp(t *testing.T) {
	s, d := newTestStore(t)
	sam := addMember(t, d, "U0SAM", "Sam")
	th := mustCreate(t, s, sam, "Thread", "go")
	replyAs(t, s, sam, th, 0, "no listener")
	s.SetReplyListener(nil)
	replyAs(t, s, sam, th, 0, "cleared listener")
}
