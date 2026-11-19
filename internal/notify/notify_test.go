package notify

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/forgeutah/forge-forum/internal/db"
	"github.com/forgeutah/forge-forum/internal/slackx"
)

// clock is a settable time source for the worker.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Unix(1_800_000_000, 0)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// openTest opens a fresh database in a temp dir for a test.
func openTest(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "forum.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// newWorker returns a worker with a silent logger. client nil means
// Slack is not configured.
func newWorker(d *db.DB, client slackx.Client, c *clock) *Worker {
	return NewWorker(d, client, slog.New(slog.NewTextHandler(io.Discard, nil)), c.Now)
}

// enqueue commits one outbox row and returns its id.
func enqueue(t *testing.T, d *db.DB, now time.Time, kind Kind, msg Message) int64 {
	t.Helper()
	ctx := context.Background()
	if err := d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		return Enqueue(ctx, tx, now, kind, msg)
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	var id int64
	if err := d.Reader().QueryRowContext(ctx, `SELECT MAX(id) FROM notifications`).Scan(&id); err != nil {
		t.Fatalf("read new row id: %v", err)
	}
	return id
}

// row is the delivery state of one outbox row.
type row struct {
	Status        string
	Attempts      int
	NextAttemptAt int64
	LastError     string
	SentAt        sql.NullInt64
}

func getRow(t *testing.T, d *db.DB, id int64) row {
	t.Helper()
	var r row
	err := d.Reader().QueryRowContext(context.Background(),
		`SELECT status, attempts, next_attempt_at, last_error, sent_at FROM notifications WHERE id = ?`, id,
	).Scan(&r.Status, &r.Attempts, &r.NextAttemptAt, &r.LastError, &r.SentAt)
	if err != nil {
		t.Fatalf("read row %d: %v", id, err)
	}
	return r
}

// runOnce runs the worker and checks how many rows reached a terminal
// state.
func runOnce(t *testing.T, w *Worker, wantDone int) {
	t.Helper()
	done, err := w.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if done != wantDone {
		t.Fatalf("RunOnce finished %d rows, want %d", done, wantDone)
	}
}

// TestEnqueue_RolledBackTransaction_LeavesNoRow checks the outbox row
// lives and dies with the caller's transaction.
func TestEnqueue_RolledBackTransaction_LeavesNoRow(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	boom := errors.New("post failed")

	err := d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		if err := Enqueue(ctx, tx, time.Unix(100, 0), KindAnnounce, Message{Channel: "C1", Text: "hi"}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("WithWriteTx error = %v, want boom", err)
	}

	var n int
	if err := d.Reader().QueryRowContext(ctx, `SELECT COUNT(*) FROM notifications`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("rows after rollback = %d, want 0", n)
	}
}

// TestEnqueue_Committed_StoresPendingRowDueNow checks the stored columns
// and the JSON payload shape.
func TestEnqueue_Committed_StoresPendingRowDueNow(t *testing.T) {
	d := openTest(t)
	id := enqueue(t, d, time.Unix(100, 0), KindLinkBack, Message{Channel: "C1", ThreadTS: "1.0", Text: "moved"})

	var kind, payload string
	var created int64
	if err := d.Reader().QueryRowContext(context.Background(),
		`SELECT kind, payload, created_at FROM notifications WHERE id = ?`, id).Scan(&kind, &payload, &created); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if kind != "link_back" || created != 100 {
		t.Errorf("kind/created_at = %q/%d, want link_back/100", kind, created)
	}
	if want := `{"channel":"C1","thread_ts":"1.0","text":"moved"}`; payload != want {
		t.Errorf("payload = %s, want %s", payload, want)
	}
	if r := getRow(t, d, id); r.Status != "pending" || r.Attempts != 0 || r.NextAttemptAt != 100 || r.SentAt.Valid {
		t.Errorf("row = %+v, want pending, no attempts, due at 100", r)
	}
}

// TestEnqueue_UnknownKind_Refused checks a kind outside the schema's set
// is an error rather than a constraint failure at commit.
func TestEnqueue_UnknownKind_Refused(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	err := d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		return Enqueue(ctx, tx, time.Unix(100, 0), Kind("digest"), Message{Channel: "C1", Text: "hi"})
	})
	if err == nil {
		t.Fatal("Enqueue with an unknown kind succeeded")
	}
}

// TestWorker_RunOnce_PendingRow_DeliveredOnceAndMarkedSent checks the
// happy path and that a second pass does not send again.
func TestWorker_RunOnce_PendingRow_DeliveredOnceAndMarkedSent(t *testing.T) {
	d, c, fake := openTest(t), newClock(), &slackx.Fake{}
	w := newWorker(d, fake, c)
	id := enqueue(t, d, c.Now(), KindLinkBack, Message{Channel: "C1", ThreadTS: "1.0", Text: "moved"})

	c.Advance(3 * time.Second)
	runOnce(t, w, 1)

	want := []slackx.PostedMessage{{Channel: "C1", Text: "moved", ThreadTS: "1.0"}}
	if got := fake.PostedMessages(); len(got) != 1 || got[0] != want[0] {
		t.Fatalf("posted = %+v, want %+v", got, want)
	}
	r := getRow(t, d, id)
	if r.Status != "sent" || r.Attempts != 1 || r.SentAt.Int64 != c.Now().Unix() || r.LastError != "" {
		t.Errorf("row = %+v, want sent at %d after 1 attempt", r, c.Now().Unix())
	}

	c.Advance(time.Hour)
	runOnce(t, w, 0)
	if n := len(fake.PostedMessages()); n != 1 {
		t.Errorf("posted after second pass = %d, want 1", n)
	}
}

// TestWorker_RunOnce_FailsTwiceThenSucceeds_OneMessageThreeAttempts
// checks retries back off 30s then 2m and deliver exactly once.
func TestWorker_RunOnce_FailsTwiceThenSucceeds_OneMessageThreeAttempts(t *testing.T) {
	d, c := openTest(t), newClock()
	fake := &slackx.Fake{PostErrs: []error{errors.New("slack is down"), errors.New("still down"), nil}}
	w := newWorker(d, fake, c)
	id := enqueue(t, d, c.Now(), KindReplyDM, Message{Channel: "U1", Text: "reply"})

	runOnce(t, w, 0)
	r := getRow(t, d, id)
	if r.Status != "pending" || r.Attempts != 1 || r.LastError != "slack is down" {
		t.Fatalf("after first failure row = %+v", r)
	}
	if want := c.Now().Add(30 * time.Second).Unix(); r.NextAttemptAt != want {
		t.Errorf("first retry at %d, want %d (30s)", r.NextAttemptAt, want)
	}

	// Not due yet: nothing is tried.
	c.Advance(29 * time.Second)
	runOnce(t, w, 0)
	if n := len(fake.Calls); n != 1 {
		t.Fatalf("Slack calls before the retry is due = %d, want 1", n)
	}

	c.Advance(time.Second)
	runOnce(t, w, 0)
	r = getRow(t, d, id)
	if r.Status != "pending" || r.Attempts != 2 || r.LastError != "still down" {
		t.Fatalf("after second failure row = %+v", r)
	}
	if want := c.Now().Add(2 * time.Minute).Unix(); r.NextAttemptAt != want {
		t.Errorf("second retry at %d, want %d (2m)", r.NextAttemptAt, want)
	}

	c.Advance(2 * time.Minute)
	runOnce(t, w, 1)
	r = getRow(t, d, id)
	if r.Status != "sent" || r.Attempts != 3 || !r.SentAt.Valid {
		t.Errorf("final row = %+v, want sent after 3 attempts", r)
	}
	if got := fake.PostedMessages(); len(got) != 1 || got[0].Channel != "U1" || got[0].Text != "reply" {
		t.Errorf("posted = %+v, want exactly the one reply", got)
	}
}

// TestWorker_RunOnce_FailsPastCap_MarkedFailedAndStops checks five failed
// attempts end the row and it is never tried again.
func TestWorker_RunOnce_FailsPastCap_MarkedFailedAndStops(t *testing.T) {
	d, c := openTest(t), newClock()
	down := errors.New("slack is down")
	fake := &slackx.Fake{PostErrs: []error{down, down, down, down, down, down, down}}
	w := newWorker(d, fake, c)
	id := enqueue(t, d, c.Now(), KindAnnounce, Message{Channel: "C1", Text: "new thread"})

	wantBackoff := []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute, 30 * time.Minute}
	for i, backoff := range wantBackoff {
		runOnce(t, w, 0)
		r := getRow(t, d, id)
		if r.Status != "pending" || r.Attempts != i+1 {
			t.Fatalf("after attempt %d row = %+v", i+1, r)
		}
		if want := c.Now().Add(backoff).Unix(); r.NextAttemptAt != want {
			t.Fatalf("after attempt %d retry at %d, want %d (%s)", i+1, r.NextAttemptAt, want, backoff)
		}
		c.Advance(backoff)
	}

	runOnce(t, w, 1)
	r := getRow(t, d, id)
	if r.Status != "failed" || r.Attempts != 5 || r.LastError != "slack is down" || r.SentAt.Valid {
		t.Fatalf("row = %+v, want failed after 5 attempts", r)
	}

	c.Advance(24 * time.Hour)
	runOnce(t, w, 0)
	if n := len(fake.Calls); n != 5 {
		t.Errorf("Slack calls = %d, want 5 (no retry after the cap)", n)
	}
	if n := len(fake.PostedMessages()); n != 0 {
		t.Errorf("posted = %d, want 0", n)
	}
}

// TestWorker_RunOnce_PermanentSlackError_FailsImmediately checks an error
// that can never succeed is not retried.
func TestWorker_RunOnce_PermanentSlackError_FailsImmediately(t *testing.T) {
	d, c := openTest(t), newClock()
	fake := &slackx.Fake{PostErrs: []error{&slackx.APIError{Code: "channel_not_found"}}}
	w := newWorker(d, fake, c)
	id := enqueue(t, d, c.Now(), KindAnnounce, Message{Channel: "C404", Text: "new thread"})

	runOnce(t, w, 1)
	if r := getRow(t, d, id); r.Status != "failed" || r.Attempts != 1 || !strings.Contains(r.LastError, "channel_not_found") {
		t.Errorf("row = %+v, want failed after 1 attempt with channel_not_found", r)
	}
}

// TestWorker_RunOnce_NilClient_MarksRowsSkipped checks that with Slack
// unconfigured every pending row ends as skipped.
func TestWorker_RunOnce_NilClient_MarksRowsSkipped(t *testing.T) {
	d, c := openTest(t), newClock()
	w := newWorker(d, nil, c)
	a := enqueue(t, d, c.Now(), KindAnnounce, Message{Channel: "C1", Text: "new thread"})
	b := enqueue(t, d, c.Now(), KindReplyDM, Message{Channel: "U1", Text: "reply"})

	runOnce(t, w, 2)
	for _, id := range []int64{a, b} {
		if r := getRow(t, d, id); r.Status != "skipped" || r.Attempts != 0 || r.SentAt.Valid {
			t.Errorf("row %d = %+v, want skipped with no attempts", id, r)
		}
	}
	runOnce(t, w, 0)
}

// TestWorker_RunOnce_RowNotYetDue_LeftAlone checks a row scheduled for
// later is not touched.
func TestWorker_RunOnce_RowNotYetDue_LeftAlone(t *testing.T) {
	d, c, fake := openTest(t), newClock(), &slackx.Fake{}
	w := newWorker(d, fake, c)
	id := enqueue(t, d, c.Now().Add(time.Minute), KindAnnounce, Message{Channel: "C1", Text: "later"})

	runOnce(t, w, 0)
	if r := getRow(t, d, id); r.Status != "pending" || r.Attempts != 0 {
		t.Errorf("row = %+v, want untouched", r)
	}
	if n := len(fake.Calls); n != 0 {
		t.Errorf("Slack calls = %d, want 0", n)
	}

	c.Advance(time.Minute)
	runOnce(t, w, 1)
}

// TestWorker_RunOnce_RateLimited_ReschedulesByAtLeastRetryAfter checks
// Slack's Retry-After wins when it is longer than the backoff, and the
// backoff wins when it is shorter.
func TestWorker_RunOnce_RateLimited_ReschedulesByAtLeastRetryAfter(t *testing.T) {
	d, c := openTest(t), newClock()
	fake := &slackx.Fake{PostErrs: []error{
		&slackx.RateLimitedError{RetryAfter: 90 * time.Second},
		&slackx.RateLimitedError{RetryAfter: 5 * time.Second},
	}}
	w := newWorker(d, fake, c)
	id := enqueue(t, d, c.Now(), KindAnnounce, Message{Channel: "C1", Text: "new thread"})

	runOnce(t, w, 0)
	r := getRow(t, d, id)
	if want := c.Now().Add(90 * time.Second).Unix(); r.Status != "pending" || r.NextAttemptAt != want {
		t.Fatalf("row = %+v, want pending and due at %d (Retry-After 90s)", r, want)
	}

	c.Advance(90 * time.Second)
	runOnce(t, w, 0)
	r = getRow(t, d, id)
	if want := c.Now().Add(2 * time.Minute).Unix(); r.NextAttemptAt != want {
		t.Errorf("row = %+v, want due at %d (2m backoff beats Retry-After 5s)", r, want)
	}
}

// TestWorker_RunOnce_AnnounceWithoutChannel_Skipped checks an announcement
// with no announcement channel configured is skipped without calling Slack.
func TestWorker_RunOnce_AnnounceWithoutChannel_Skipped(t *testing.T) {
	d, c, fake := openTest(t), newClock(), &slackx.Fake{}
	w := newWorker(d, fake, c)
	id := enqueue(t, d, c.Now(), KindAnnounce, Message{Text: "new thread"})

	runOnce(t, w, 1)
	if r := getRow(t, d, id); r.Status != "skipped" || r.Attempts != 0 {
		t.Errorf("row = %+v, want skipped", r)
	}
	if n := len(fake.Calls); n != 0 {
		t.Errorf("Slack calls = %d, want 0", n)
	}
}

// TestWorker_RunOnce_UnusableRows_FailedWithoutCallingSlack checks a
// payload that cannot be delivered ends as failed instead of blocking
// the queue.
func TestWorker_RunOnce_UnusableRows_FailedWithoutCallingSlack(t *testing.T) {
	d, c, fake := openTest(t), newClock(), &slackx.Fake{}
	w := newWorker(d, fake, c)
	ctx := context.Background()
	noChannel := enqueue(t, d, c.Now(), KindReplyDM, Message{Text: "reply"})
	if err := d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO notifications (kind, payload, next_attempt_at, created_at) VALUES ('announce', 'not json', ?, ?)`,
			c.Now().Unix(), c.Now().Unix())
		return err
	}); err != nil {
		t.Fatalf("insert bad row: %v", err)
	}
	good := enqueue(t, d, c.Now(), KindAnnounce, Message{Channel: "C1", Text: "new thread"})

	runOnce(t, w, 3)
	for _, id := range []int64{noChannel, noChannel + 1} {
		if r := getRow(t, d, id); r.Status != "failed" || r.LastError == "" {
			t.Errorf("row %d = %+v, want failed with a reason", id, r)
		}
	}
	if r := getRow(t, d, good); r.Status != "sent" {
		t.Errorf("good row = %+v, want sent", r)
	}
	if n := len(fake.Calls); n != 1 {
		t.Errorf("Slack calls = %d, want 1", n)
	}
}

// TestWorker_RunOnce_ManyRows_DeliveredOldestFirst checks a queue larger
// than one batch drains completely and in order.
func TestWorker_RunOnce_ManyRows_DeliveredOldestFirst(t *testing.T) {
	d, c, fake := openTest(t), newClock(), &slackx.Fake{}
	w := newWorker(d, fake, c)
	const n = batchSize*2 + 3
	for i := range n {
		enqueue(t, d, c.Now(), KindReplyDM, Message{Channel: "U1", Text: string(rune('a' + i%26))})
	}

	runOnce(t, w, n)
	posted := fake.PostedMessages()
	if len(posted) != n {
		t.Fatalf("posted = %d, want %d", len(posted), n)
	}
	for i, p := range posted {
		if want := string(rune('a' + i%26)); p.Text != want {
			t.Fatalf("message %d = %q, want %q (oldest first)", i, p.Text, want)
		}
	}
}

// TestWorker_Run_WakeDeliversWithoutWaitingForTheInterval checks Wake
// triggers a pass and Run stops when its context is cancelled.
func TestWorker_Run_WakeDeliversWithoutWaitingForTheInterval(t *testing.T) {
	d, c, fake := openTest(t), newClock(), &slackx.Fake{}
	w := newWorker(d, fake, c)
	w.interval = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(stopped)
	}()

	waitSent := func(id int64) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for getRow(t, d, id).Status != "sent" {
			if time.Now().After(deadline) {
				t.Fatalf("row %d was not sent within 5s", id)
			}
			w.Wake()
			time.Sleep(5 * time.Millisecond)
		}
	}
	waitSent(enqueue(t, d, c.Now(), KindAnnounce, Message{Channel: "C1", Text: "one"}))
	waitSent(enqueue(t, d, c.Now(), KindAnnounce, Message{Channel: "C1", Text: "two"}))

	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
	if n := len(fake.PostedMessages()); n != 2 {
		t.Errorf("posted = %d, want 2", n)
	}
}

// TestAnnounceText_TypicalThread_ExactWording checks the announcement
// against the Slack touchpoints design.
func TestAnnounceText_TypicalThread_ExactWording(t *testing.T) {
	got := AnnounceText("U1", "How are you evaluating coding agents on real repositories?", "https://forum.test/t/7", []string{"ai", "go"})
	want := "<@U1> started a thread.\n" +
		"<https://forum.test/t/7|How are you evaluating coding agents on real repositories? →>\n" +
		"#ai #go"
	if got != want {
		t.Errorf("AnnounceText =\n%s\nwant\n%s", got, want)
	}

	if got := AnnounceText("U1", "Untagged", "https://forum.test/t/8", nil); got != "<@U1> started a thread.\n<https://forum.test/t/8|Untagged →>" {
		t.Errorf("AnnounceText without tags =\n%s", got)
	}
}

// TestReplyDMText_TypicalReply_ExactWording checks the direct message
// against the Slack touchpoints design.
func TestReplyDMText_TypicalReply_ExactWording(t *testing.T) {
	got := ReplyDMText("Dev Ortiz", "Who is building software factories?",
		"How do you decide a ticket is scoped tightly enough?\n\nWe keep getting half-finished branches.",
		"https://forum.test/t/3#reply-9")
	want := "Dev Ortiz replied to you in *Who is building software factories?*\n" +
		"> How do you decide a ticket is scoped tightly enough? We keep getting half-finished branches.\n" +
		"<https://forum.test/t/3#reply-9|Read and reply →>\n" +
		"Turn these off in your forum settings."
	if got != want {
		t.Errorf("ReplyDMText =\n%s\nwant\n%s", got, want)
	}
}

// TestReplyDMText_LongExcerpt_TruncatedWithEllipsis checks the quoted
// excerpt is capped at 280 characters, counted before escaping.
func TestReplyDMText_LongExcerpt_TruncatedWithEllipsis(t *testing.T) {
	got := ReplyDMText("Dana", "Title", strings.Repeat("<é", 300), "https://forum.test/r")
	quote := strings.Split(got, "\n")[1]
	want := "> " + strings.Repeat("&lt;é", 139) + "&lt;…"
	if quote != want {
		t.Errorf("quote = %q\nwant  %q", quote, want)
	}

	short := strings.Repeat("a", 280)
	if got := ReplyDMText("Dana", "Title", short, "https://forum.test/r"); !strings.Contains(got, "> "+short+"\n") {
		t.Errorf("a 280 character excerpt was altered:\n%s", got)
	}
}

// TestLinkBackText_Counts_SingularPluralZero checks the reply counts read
// cleanly at zero, one, and many.
func TestLinkBackText_Counts_SingularPluralZero(t *testing.T) {
	cases := []struct {
		imported, remaining int
		want                string
	}{
		{12, 38, "12 replies that answer the original post went with it. 38 replies stayed here."},
		{1, 1, "1 reply that answers the original post went with it. 1 reply stayed here."},
		{0, 4, "No replies went with it. 4 replies stayed here."},
		{3, 0, "3 replies that answer the original post went with it. No replies stayed here."},
		{0, 0, "No replies went with it. No replies stayed here."},
	}
	for _, tc := range cases {
		got := LinkBackText("U2", tc.imported, tc.remaining, "Who is building software factories?", "https://forum.test/t/3")
		want := "<@U2> moved this thread to the forum.\n" +
			tc.want + "\n" +
			"<https://forum.test/t/3|Who is building software factories? →>\n" +
			"Carry on there: replies nest and the best ones rise."
		if got != want {
			t.Errorf("LinkBackText(%d, %d) =\n%s\nwant\n%s", tc.imported, tc.remaining, got, want)
		}
	}
}

// TestMoveFailedText_ExactWording checks the failure notice, with and
// without a title.
func TestMoveFailedText_ExactWording(t *testing.T) {
	if got, want := MoveFailedText("Who is <building>?"),
		"Moving *Who is &lt;building&gt;?* to the forum did not work. Nothing was imported and the thread here is unchanged. You can try again."; got != want {
		t.Errorf("MoveFailedText = %q, want %q", got, want)
	}
	if got, want := MoveFailedText(" "),
		"Moving this thread to the forum did not work. Nothing was imported and the thread here is unchanged. You can try again."; got != want {
		t.Errorf("MoveFailedText without a title = %q, want %q", got, want)
	}
}

// TestTextBuilders_MemberText_CannotInjectSlackMarkup checks <!channel>,
// user mentions, and ampersands in member text arrive escaped.
func TestTextBuilders_MemberText_CannotInjectSlackMarkup(t *testing.T) {
	const hostile = "Ops & you <!channel> <@U1> <https://evil.test|click>"
	const escaped = "Ops &amp; you &lt;!channel&gt; &lt;@U1&gt; &lt;https://evil.test|click&gt;"

	texts := map[string]string{
		"AnnounceText title":   AnnounceText("U9", hostile, "https://forum.test/t/1", []string{"<!here>"}),
		"ReplyDMText name":     ReplyDMText(hostile, "Title", "excerpt", "https://forum.test/r"),
		"ReplyDMText title":    ReplyDMText("Dana", hostile, "excerpt", "https://forum.test/r"),
		"ReplyDMText excerpt":  ReplyDMText("Dana", "Title", hostile, "https://forum.test/r"),
		"LinkBackText title":   LinkBackText("U9", 1, 1, hostile, "https://forum.test/t/1"),
		"MoveFailedText title": MoveFailedText(hostile),
	}
	for name, text := range texts {
		if !strings.Contains(text, escaped) {
			t.Errorf("%s: member text not escaped:\n%s", name, text)
		}
		for _, raw := range []string{"<!channel>", "<@U1>", "<!here>", "<https://evil.test"} {
			if strings.Contains(text, raw) {
				t.Errorf("%s: contains raw %q:\n%s", name, raw, text)
			}
		}
	}

	// A title cannot break out of its line either.
	if got := AnnounceText("U9", "one\ntwo", "https://forum.test/t/1", nil); strings.Count(got, "\n") != 1 {
		t.Errorf("newline in a title survived:\n%s", got)
	}
}

// TestTextBuilders_CopyRules_NoExclamationMarks checks the fixed wording
// follows the copy rules.
func TestTextBuilders_CopyRules_NoExclamationMarks(t *testing.T) {
	for _, text := range []string{
		AnnounceText("U1", "t", "https://forum.test/t/1", []string{"ai"}),
		ReplyDMText("Dana", "t", "e", "https://forum.test/r"),
		LinkBackText("U1", 2, 2, "t", "https://forum.test/t/1"),
		MoveFailedText("t"),
	} {
		if strings.Contains(text, "!") {
			t.Errorf("exclamation mark in %q", text)
		}
	}
}

// cancellingClient is a Slack client whose accepted post is followed at
// once by the worker's context being cancelled, as when shutdown lands
// between the send and the row being marked.
type cancellingClient struct {
	*slackx.Fake
	cancel context.CancelFunc
}

func (c cancellingClient) PostMessage(ctx context.Context, channel, text, threadTS string) error {
	err := c.Fake.PostMessage(ctx, channel, text, threadTS)
	c.cancel()
	return err
}

// TestWorker_RunOnce_CancelledAfterSend_StillMarkedSent checks a message
// Slack accepted is not left pending, and so sent twice, because shutdown
// cancelled the context before the row was marked.
func TestWorker_RunOnce_CancelledAfterSend_StillMarkedSent(t *testing.T) {
	d, c, fake := openTest(t), newClock(), &slackx.Fake{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := newWorker(d, cancellingClient{Fake: fake, cancel: cancel}, c)
	id := enqueue(t, d, c.Now(), KindMoveFailed, Message{Channel: "U1", Text: "did not work"})

	// The pass may report the cancellation; what matters is the row.
	done, _ := w.RunOnce(ctx)
	if ctx.Err() == nil {
		t.Fatal("the context was not cancelled during the send")
	}
	if done != 1 {
		t.Errorf("RunOnce finished %d rows, want 1", done)
	}
	if r := getRow(t, d, id); r.Status != "sent" || r.Attempts != 1 || r.SentAt.Int64 != c.Now().Unix() {
		t.Errorf("row = %+v, want sent after 1 attempt", r)
	}

	// After the restart nothing is left to send.
	c.Advance(time.Hour)
	runOnce(t, newWorker(d, fake, c), 0)
	if n := len(fake.PostedMessages()); n != 1 {
		t.Errorf("posted = %d, want exactly 1", n)
	}
}

// TestWorker_RunOnce_CancelledDuringFailedSend_AttemptRecorded checks a
// failed attempt is still written when the context ends while the worker
// records it.
func TestWorker_RunOnce_CancelledDuringFailedSend_AttemptRecorded(t *testing.T) {
	d, c := openTest(t), newClock()
	w := newWorker(d, &slackx.Fake{}, c)
	id := enqueue(t, d, c.Now(), KindReplyDM, Message{Channel: "U1", Text: "reply"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	next := c.Now().Add(30 * time.Second)
	if err := w.reschedule(ctx, outboxRow{id: id, kind: string(KindReplyDM)}, 1, "slack is down", next); err != nil {
		t.Fatalf("reschedule: %v", err)
	}
	if r := getRow(t, d, id); r.Status != "pending" || r.Attempts != 1 || r.NextAttemptAt != next.Unix() || r.LastError != "slack is down" {
		t.Errorf("row = %+v, want pending with 1 attempt recorded", r)
	}
}
