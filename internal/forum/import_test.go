package forum

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/forgeutah/forge-forum/internal/db"
	"github.com/forgeutah/forge-forum/internal/notify"
)

var importPostedAt = time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)

// importInput returns a valid move of a two-reply Slack thread.
func importInput() ImportInput {
	return ImportInput{
		Title:         "How do we roll out Gateway API?",
		TagSlugs:      []string{"kubernetes"},
		MoverSlackID:  "UMOVER",
		MoverName:     "Mo Mover",
		ChannelID:     "C1",
		ChannelName:   "kubernetes",
		ThreadTS:      "1760950800.000100",
		Permalink:     "https://slack.test/archives/C1/p1760950800000100",
		AuthorSlackID: "UASKER",
		AuthorName:    "Ada Asker",
		BodyMD:        "Has anyone moved from Ingress to **Gateway API**?",
		PostedAt:      importPostedAt,
		Replies: []ImportedReply{
			{SlackUserID: "UR1", AuthorName: "Rae One", BodyMD: "Yes, last quarter.", SlackTS: "1760950860.000200", CreatedAt: importPostedAt.Add(time.Minute)},
			{SlackUserID: "UASKER", AuthorName: "Ada Asker", BodyMD: "Adding detail.", SlackTS: "1760950920.000300", CreatedAt: importPostedAt.Add(2 * time.Minute)},
		},
		Remaining: 3,
		ThreadURL: func(id int64) string { return fmt.Sprintf("https://forum.test/threads/%d", id) },
	}
}

// importCount returns the number of rows in table.
func importCount(t *testing.T, d *db.DB, table string) int {
	t.Helper()
	var n int
	if err := d.Reader().QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// TestStore_ImportSlackThread_CreatesThreadRepliesAndLinkBack checks the whole move lands in one go.
func TestStore_ImportSlackThread_CreatesThreadRepliesAndLinkBack(t *testing.T) {
	s, d := newTestStore(t)
	ctx := context.Background()
	in := importInput()

	id, existing, err := s.ImportSlackThread(ctx, in)
	if err != nil || existing {
		t.Fatalf("ImportSlackThread = %d, %v, %v; want a new thread", id, existing, err)
	}

	th, err := s.GetThread(ctx, id, 0)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if th.Title != in.Title || th.BodyMD != in.BodyMD || th.AuthorName != "Ada Asker" {
		t.Errorf("thread = %q / %q by %q", th.Title, th.BodyMD, th.AuthorName)
	}
	if !th.FromSlack || th.SlackChannelName != "kubernetes" || th.SlackPermalink != in.Permalink || th.SlackRemaining != 3 {
		t.Errorf("slack fields = %v %q %q %d", th.FromSlack, th.SlackChannelName, th.SlackPermalink, th.SlackRemaining)
	}
	if th.ReplyCount != 2 {
		t.Errorf("ReplyCount = %d, want 2", th.ReplyCount)
	}
	if !th.CreatedAt.Equal(importPostedAt) {
		t.Errorf("CreatedAt = %v, want the Slack post time %v", th.CreatedAt, importPostedAt)
	}
	if !th.LastActivityAt.After(importPostedAt) {
		t.Errorf("LastActivityAt = %v, want the time of the move", th.LastActivityAt)
	}

	var moverSlack string
	var moverForge sql.NullInt64
	if err := d.Reader().QueryRow(`SELECT m.slack_user_id, m.forge_user_id FROM threads t
		JOIN members m ON m.id = t.moved_by WHERE t.id = ?`, id).Scan(&moverSlack, &moverForge); err != nil {
		t.Fatalf("mover: %v", err)
	}
	if moverSlack != "UMOVER" || moverForge.Valid {
		t.Errorf("mover = %q forge=%v, want UMOVER with no Forge user", moverSlack, moverForge)
	}

	rows, err := d.Reader().Query(`SELECT m.slack_user_id, r.body_md, r.slack_ts, r.created_at, r.imported, r.parent_id
		FROM replies r JOIN members m ON m.id = r.author_id WHERE r.thread_id = ? ORDER BY r.id`, id)
	if err != nil {
		t.Fatalf("replies: %v", err)
	}
	defer rows.Close()
	i := 0
	for rows.Next() {
		var slackID, body, ts string
		var created int64
		var imported bool
		var parent sql.NullInt64
		if err := rows.Scan(&slackID, &body, &ts, &created, &imported, &parent); err != nil {
			t.Fatalf("scan reply: %v", err)
		}
		want := in.Replies[i]
		if slackID != want.SlackUserID || body != want.BodyMD || ts != want.SlackTS || created != want.CreatedAt.Unix() {
			t.Errorf("reply %d = %q %q %q %d, want %+v", i, slackID, body, ts, created, want)
		}
		if !imported || parent.Valid {
			t.Errorf("reply %d imported=%v parent=%v, want imported first-level", i, imported, parent)
		}
		i++
	}
	if i != 2 {
		t.Fatalf("imported %d replies, want 2", i)
	}
	if n := importCount(t, d, "members"); n != 3 {
		t.Errorf("members = %d, want 3 (asker, mover, one replier)", n)
	}

	var kind, payload string
	if err := d.Reader().QueryRow(`SELECT kind, payload FROM notifications`).Scan(&kind, &payload); err != nil {
		t.Fatalf("notification: %v", err)
	}
	if kind != "link_back" {
		t.Errorf("kind = %q, want link_back", kind)
	}
	var msg notify.Message
	if err := json.Unmarshal([]byte(payload), &msg); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if msg.Channel != "C1" || msg.ThreadTS != "1760950800.000100" {
		t.Errorf("link-back goes to %q thread %q, want the source Slack thread", msg.Channel, msg.ThreadTS)
	}
	for _, want := range []string{"<@UMOVER>", fmt.Sprintf("https://forum.test/threads/%d", id), "3 replies stayed here"} {
		if !strings.Contains(msg.Text, want) {
			t.Errorf("link-back text %q lacks %q", msg.Text, want)
		}
	}
	if n := importCount(t, d, "notifications"); n != 1 {
		t.Errorf("notifications = %d, want 1", n)
	}
}

// TestStore_ImportSlackThread_SecondMoveReturnsExisting checks a moved Slack thread is never imported twice.
func TestStore_ImportSlackThread_SecondMoveReturnsExisting(t *testing.T) {
	s, d := newTestStore(t)
	ctx := context.Background()
	first, _, err := s.ImportSlackThread(ctx, importInput())
	if err != nil {
		t.Fatalf("first import: %v", err)
	}

	again := importInput()
	again.Title = "A different title"
	id, existing, err := s.ImportSlackThread(ctx, again)
	if err != nil || !existing || id != first {
		t.Fatalf("second import = %d, %v, %v; want %d, true, nil", id, existing, err, first)
	}
	if th, n, r, m := importCount(t, d, "threads"), importCount(t, d, "notifications"), importCount(t, d, "replies"), importCount(t, d, "members"); th != 1 || n != 1 || r != 2 || m != 3 {
		t.Errorf("threads=%d notifications=%d replies=%d members=%d, want 1, 1, 2, 3", th, n, r, m)
	}

	got, ok, err := s.SlackThreadID(ctx, "C1", "1760950800.000100")
	if err != nil || !ok || got != first {
		t.Errorf("SlackThreadID = %d, %v, %v; want %d, true, nil", got, ok, err, first)
	}
	if _, ok, err := s.SlackThreadID(ctx, "C1", "1.0"); err != nil || ok {
		t.Errorf("SlackThreadID for an unmoved thread = %v, %v; want false, nil", ok, err)
	}
}

// TestStore_ImportSlackThread_RaceYieldsOneThread checks two moves of one Slack thread at once create one forum thread.
func TestStore_ImportSlackThread_RaceYieldsOneThread(t *testing.T) {
	s, d := newTestStore(t)
	const movers = 4
	ids := make([]int64, movers)
	existing := make([]bool, movers)
	errs := make([]error, movers)
	var wg sync.WaitGroup
	for i := range movers {
		wg.Go(func() {
			ids[i], existing[i], errs[i] = s.ImportSlackThread(context.Background(), importInput())
		})
	}
	wg.Wait()

	created := 0
	for i := range movers {
		if errs[i] != nil {
			t.Fatalf("import %d: %v", i, errs[i])
		}
		if ids[i] != ids[0] {
			t.Errorf("import %d returned thread %d, want %d", i, ids[i], ids[0])
		}
		if !existing[i] {
			created++
		}
	}
	if created != 1 {
		t.Errorf("%d imports reported a new thread, want 1", created)
	}
	if th, n := importCount(t, d, "threads"), importCount(t, d, "notifications"); th != 1 || n != 1 {
		t.Errorf("threads=%d notifications=%d, want 1 and 1", th, n)
	}
}

// TestStore_ImportSlackThread_UniqueIndexLoserGetsExisting checks the unique index, not only the lookup, stops a duplicate.
func TestStore_ImportSlackThread_UniqueIndexLoserGetsExisting(t *testing.T) {
	s, d := newTestStore(t)
	ctx := context.Background()
	first, _, err := s.ImportSlackThread(ctx, importInput())
	if err != nil {
		t.Fatalf("first import: %v", err)
	}

	// Insert past the lookup, as a racing move would, and let the index fire.
	var id int64
	var existing bool
	err = d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		var err error
		id, err = s.importInsertTx(ctx, tx, importInput())
		return err
	})
	if err == nil {
		t.Fatal("a second insert for the same Slack thread succeeded; the unique index did not fire")
	}
	id, existing, err = s.importResolveConflict(ctx, importInput(), err)
	if err != nil || !existing || id != first {
		t.Fatalf("after the index fired = %d, %v, %v; want %d, true, nil", id, existing, err, first)
	}
	if n := importCount(t, d, "threads"); n != 1 {
		t.Errorf("threads = %d, want 1", n)
	}
}

// TestStore_ImportSlackThread_RemovedThreadStillCountsAsMoved checks a removed forum thread still blocks a second move.
func TestStore_ImportSlackThread_RemovedThreadStillCountsAsMoved(t *testing.T) {
	s, d := newTestStore(t)
	ctx := context.Background()
	first, _, err := s.ImportSlackThread(ctx, importInput())
	if err != nil {
		t.Fatalf("first import: %v", err)
	}
	err = d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE threads SET deleted_at = 1 WHERE id = ?`, first)
		return err
	})
	if err != nil {
		t.Fatalf("remove thread: %v", err)
	}
	id, existing, err := s.ImportSlackThread(ctx, importInput())
	if err != nil || !existing || id != first {
		t.Fatalf("import after removal = %d, %v, %v; want %d, true, nil", id, existing, err, first)
	}
}

// TestStore_ImportSlackThread_ValidationWritesNothing checks bad input is refused whole, with no stray members or rows.
func TestStore_ImportSlackThread_ValidationWritesNothing(t *testing.T) {
	cases := []struct {
		name  string
		edit  func(*ImportInput)
		field string
	}{
		{"empty title", func(in *ImportInput) { in.Title = "  " }, "title"},
		{"no tags", func(in *ImportInput) { in.TagSlugs = nil }, "tags"},
		{"unknown tag", func(in *ImportInput) { in.TagSlugs = []string{"nope"} }, "tags"},
		{"empty body", func(in *ImportInput) { in.BodyMD = "" }, "body"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, d := newTestStore(t)
			in := importInput()
			tc.edit(&in)
			_, _, err := s.ImportSlackThread(context.Background(), in)
			var ve ValidationError
			if !errors.As(err, &ve) || ve.Field != tc.field {
				t.Fatalf("err = %v, want a ValidationError on %q", err, tc.field)
			}
			for _, table := range []string{"threads", "replies", "members", "notifications"} {
				if n := importCount(t, d, table); n != 0 {
					t.Errorf("%s = %d rows after a refused import, want 0", table, n)
				}
			}
		})
	}
}

// TestStore_ImportSlackThread_KeepsExistingMemberProfile checks an author who already signed in keeps their own name.
func TestStore_ImportSlackThread_KeepsExistingMemberProfile(t *testing.T) {
	s, d := newTestStore(t)
	ctx := context.Background()
	asker := addMember(t, d, "UASKER", "Ada (signed in)")

	id, _, err := s.ImportSlackThread(ctx, importInput())
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	th, err := s.GetThread(ctx, id, 0)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if th.AuthorID != asker || th.AuthorName != "Ada (signed in)" {
		t.Errorf("author = %d %q, want the existing member %d", th.AuthorID, th.AuthorName, asker)
	}
}
