package forum

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/forgeutah/forge-forum/internal/db"
	"github.com/forgeutah/forge-forum/internal/notify"
	"github.com/forgeutah/forge-forum/internal/slackx"
)

// outboxRow is one notifications row with its payload decoded.
type outboxRow struct {
	Kind   notify.Kind
	Status string
	notify.Message
}

func outbox(t *testing.T, d *db.DB) []outboxRow {
	t.Helper()
	rows, err := d.Reader().Query(`SELECT kind, status, payload FROM notifications ORDER BY id`)
	if err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	defer rows.Close()
	var out []outboxRow
	for rows.Next() {
		var r outboxRow
		var payload string
		if err := rows.Scan(&r.Kind, &r.Status, &payload); err != nil {
			t.Fatalf("read outbox: %v", err)
		}
		if err := json.Unmarshal([]byte(payload), &r.Message); err != nil {
			t.Fatalf("decode payload %q: %v", payload, err)
		}
		out = append(out, r)
	}
	return out
}

// notifying turns the store's notifications on and returns a counter of
// Wake calls.
func notifying(s *Store) *int {
	wakes := new(int)
	s.SetNotifier(Notifier{BaseURL: "https://forum.example.com", AnnounceChannel: "C0ANNOUNCE", Wake: func() { *wakes++ }})
	return wakes
}

// TestStore_CreateThread_NotifierSet_EnqueuesOneAnnouncement covers R11:
// one announce row carrying the author, title, tags, and link, and a wake
// that happens only after the row is committed.
func TestStore_CreateThread_NotifierSet_EnqueuesOneAnnouncement(t *testing.T) {
	s, d := newTestStore(t)
	sam := addMember(t, d, "U0SAM", "Sam")
	var seenAtWake []int
	s.SetNotifier(Notifier{BaseURL: "https://forum.example.com", AnnounceChannel: "C0ANNOUNCE", Wake: func() {
		// The reader pool sees only committed rows.
		seenAtWake = append(seenAtWake, countRows(t, d, "notifications"))
	}})

	id := mustCreate(t, s, sam, "Who is building software factories?", "ai", "architecture", "ai")

	rows := outbox(t, d)
	if len(rows) != 1 {
		t.Fatalf("outbox has %d rows, want 1: %+v", len(rows), rows)
	}
	want := notify.AnnounceText("U0SAM", "Who is building software factories?",
		fmt.Sprintf("https://forum.example.com/threads/%d", id), []string{"ai", "architecture"})
	got := rows[0]
	if got.Kind != notify.KindAnnounce || got.Status != "pending" || got.Channel != "C0ANNOUNCE" || got.ThreadTS != "" || got.Text != want {
		t.Errorf("row = %+v\nwant text %q", got, want)
	}
	if len(seenAtWake) != 1 || seenAtWake[0] != 1 {
		t.Errorf("Wake saw %v committed rows per call, want one call seeing 1", seenAtWake)
	}
}

// TestStore_CreateThread_NoAnnounceChannel_StillEnqueued checks the row is
// written for the worker to mark skipped, so the outbox stays the record.
func TestStore_CreateThread_NoAnnounceChannel_StillEnqueued(t *testing.T) {
	s, d := newTestStore(t)
	sam := addMember(t, d, "U0SAM", "Sam")
	s.SetNotifier(Notifier{BaseURL: "https://forum.example.com"})
	mustCreate(t, s, sam, "Thread", "go")

	rows := outbox(t, d)
	if len(rows) != 1 || rows[0].Kind != notify.KindAnnounce || rows[0].Channel != "" {
		t.Errorf("outbox = %+v, want one announce row with no channel", rows)
	}
}

// TestStore_CreateReply_AE3_OnlyTheAnsweredAuthorIsMessaged covers AE3:
// Lee answers Dana's reply in Sam's thread, so Dana is messaged and Sam is
// not; with Dana's messages off, nobody is.
func TestStore_CreateReply_AE3_OnlyTheAnsweredAuthorIsMessaged(t *testing.T) {
	s, d := newTestStore(t)
	sam := addMember(t, d, "U0SAM", "Sam")
	dana := addMember(t, d, "U0DANA", "Dana")
	lee := addMember(t, d, "U0LEE", "Lee W")
	th := mustCreate(t, s, sam, "Factories", "ai")
	danas := replyAs(t, s, dana, th, 0, "We run a small one.")
	wakes := notifying(s)

	lees := replyAs(t, s, lee, th, danas, "How do you scope\ntickets?")

	rows := outbox(t, d)
	if len(rows) != 1 {
		t.Fatalf("outbox has %d rows, want 1: %+v", len(rows), rows)
	}
	want := notify.ReplyDMText("Lee W", "Factories", "How do you scope\ntickets?",
		fmt.Sprintf("https://forum.example.com/threads/%d#reply-%d", th, lees))
	if got := rows[0]; got.Kind != notify.KindReplyDM || got.Channel != "U0DANA" || got.Text != want {
		t.Errorf("row = %+v\nwant a reply_dm to U0DANA with text %q", got, want)
	}
	if *wakes != 1 {
		t.Errorf("Wake called %d times, want 1", *wakes)
	}

	if err := s.SetDMReplies(context.Background(), dana, false); err != nil {
		t.Fatal(err)
	}
	replyAs(t, s, lee, th, danas, "Asking again.")
	if rows := outbox(t, d); len(rows) != 1 {
		t.Errorf("outbox has %d rows after a reply to a member with messages off, want still 1", len(rows))
	}
}

// TestStore_CreateReply_TopLevel_MessagesThreadAuthor checks a reply to
// the thread itself goes to the thread's author.
func TestStore_CreateReply_TopLevel_MessagesThreadAuthor(t *testing.T) {
	s, d := newTestStore(t)
	sam := addMember(t, d, "U0SAM", "Sam")
	dana := addMember(t, d, "U0DANA", "Dana")
	th := mustCreate(t, s, sam, "Factories", "ai")
	notifying(s)

	replyAs(t, s, dana, th, 0, "We run one.")
	rows := outbox(t, d)
	if len(rows) != 1 || rows[0].Kind != notify.KindReplyDM || rows[0].Channel != "U0SAM" {
		t.Errorf("outbox = %+v, want one reply_dm to U0SAM", rows)
	}
}

// TestStore_CreateReply_OwnPost_EnqueuesNothing covers the second half of
// R12 for both a member's own thread and their own reply.
func TestStore_CreateReply_OwnPost_EnqueuesNothing(t *testing.T) {
	s, d := newTestStore(t)
	sam := addMember(t, d, "U0SAM", "Sam")
	th := mustCreate(t, s, sam, "Factories", "ai")
	wakes := notifying(s)

	own := replyAs(t, s, sam, th, 0, "Adding context to my thread.")
	replyAs(t, s, sam, th, own, "And to my own reply.")
	if rows := outbox(t, d); len(rows) != 0 {
		t.Errorf("outbox = %+v, want empty", rows)
	}
	if *wakes != 0 {
		t.Errorf("Wake called %d times with nothing enqueued, want 0", *wakes)
	}
}

// TestStore_Posts_RefusedOrRolledBack_EnqueueNothing checks the outbox row
// lives and dies with its post (KTD6).
func TestStore_Posts_RefusedOrRolledBack_EnqueueNothing(t *testing.T) {
	s, d := newTestStore(t)
	ctx := context.Background()
	sam := addMember(t, d, "U0SAM", "Sam")
	dana := addMember(t, d, "U0DANA", "Dana")
	th := mustCreate(t, s, sam, "Factories", "ai")
	wakes := notifying(s)

	if _, err := s.CreateThread(ctx, sam, "No tags", "Body", nil); err == nil {
		t.Fatal("thread without tags accepted")
	}
	if _, err := s.CreateReply(ctx, dana, th, 0, "  "); err == nil {
		t.Fatal("empty reply accepted")
	}
	if _, err := s.CreateReply(ctx, dana, th, 999, "to nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reply to a missing parent: %v", err)
	}

	// A thread whose announcement cannot be stored is not stored either.
	exec(t, d, `CREATE TRIGGER outbox_down BEFORE INSERT ON notifications
		BEGIN SELECT RAISE(ABORT, 'outbox down'); END`)
	if _, err := s.CreateThread(ctx, sam, "Lost thread", "Body", []string{"go"}); err == nil {
		t.Error("thread stored although its announcement could not be")
	}
	if _, err := s.CreateReply(ctx, dana, th, 0, "Lost reply"); err == nil {
		t.Error("reply stored although its message could not be")
	}
	exec(t, d, `DROP TRIGGER outbox_down`)

	if n, m, r := countRows(t, d, "notifications"), countRows(t, d, "threads"), countRows(t, d, "replies"); n != 0 || m != 1 || r != 0 {
		t.Errorf("notifications = %d, threads = %d, replies = %d, want 0, 1, 0", n, m, r)
	}
	if got := threadOf(t, s, th, 0).ReplyCount; got != 0 {
		t.Errorf("reply_count = %d, want 0", got)
	}
	if *wakes != 0 {
		t.Errorf("Wake called %d times, want 0", *wakes)
	}
}

// TestStore_NotifierUnset_PostsSucceedAndEnqueueNothing checks the zero
// Notifier means notifications are off.
func TestStore_NotifierUnset_PostsSucceedAndEnqueueNothing(t *testing.T) {
	s, d := newTestStore(t)
	sam := addMember(t, d, "U0SAM", "Sam")
	dana := addMember(t, d, "U0DANA", "Dana")
	th := mustCreate(t, s, sam, "Factories", "ai")
	replyAs(t, s, dana, th, 0, "Reply")

	wakes := notifying(s)
	s.SetNotifier(Notifier{})
	replyAs(t, s, dana, th, 0, "Another")
	mustCreate(t, s, sam, "Second thread", "go")
	if n := countRows(t, d, "notifications"); n != 0 || *wakes != 0 {
		t.Errorf("notifications = %d, wakes = %d, want 0 and 0", n, *wakes)
	}
}

// TestStore_CreateThreadTx_DoesNotAnnounce guards the importer's path: it
// creates threads through createThreadTx and posts its own link-back, so
// that helper must stay silent (R18).
func TestStore_CreateThreadTx_DoesNotAnnounce(t *testing.T) {
	s, d := newTestStore(t)
	sam := addMember(t, d, "U0SAM", "Sam")
	notifying(s)
	err := d.WithWriteTx(context.Background(), func(tx *sql.Tx) error {
		_, err := s.createThreadTx(context.Background(), tx, newThread{AuthorID: sam, Title: "Moved", BodyMD: "Body", TagSlugs: []string{"go"}})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, d, "notifications"); n != 0 {
		t.Errorf("notifications = %d, want 0", n)
	}
}

// TestStore_Posts_RealWorker_FakeSlackReceivesBoth runs the real outbox
// worker over a thread and a reply and checks what Slack is sent.
func TestStore_Posts_RealWorker_FakeSlackReceivesBoth(t *testing.T) {
	s, d := newTestStore(t)
	sam := addMember(t, d, "U0SAM", "Sam")
	dana := addMember(t, d, "U0DANA", "Dana")
	fake := &slackx.Fake{}
	worker := notify.NewWorker(d, fake, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Now)
	s.SetNotifier(Notifier{BaseURL: "https://forum.example.com", AnnounceChannel: "C0ANNOUNCE", Wake: worker.Wake})

	th := mustCreate(t, s, sam, "Factories", "ai")
	r := replyAs(t, s, dana, th, 0, "We run one.")
	if _, err := worker.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	posted := fake.PostedMessages()
	if len(posted) != 2 {
		t.Fatalf("Slack received %d messages, want 2: %+v", len(posted), posted)
	}
	wantAnnounce := notify.AnnounceText("U0SAM", "Factories", fmt.Sprintf("https://forum.example.com/threads/%d", th), []string{"ai"})
	wantDM := notify.ReplyDMText("Dana", "Factories", "We run one.", fmt.Sprintf("https://forum.example.com/threads/%d#reply-%d", th, r))
	if posted[0] != (slackx.PostedMessage{Channel: "C0ANNOUNCE", Text: wantAnnounce}) {
		t.Errorf("announcement = %+v", posted[0])
	}
	if posted[1] != (slackx.PostedMessage{Channel: "U0SAM", Text: wantDM}) {
		t.Errorf("direct message = %+v", posted[1])
	}
	for _, row := range outbox(t, d) {
		if row.Status != "sent" {
			t.Errorf("%s row is %q, want sent", row.Kind, row.Status)
		}
	}
}
