package move

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/forgeutah/forge-forum/internal/db"
	"github.com/forgeutah/forge-forum/internal/forum"
	"github.com/forgeutah/forge-forum/internal/identity"
	"github.com/forgeutah/forge-forum/internal/notify"
	"github.com/forgeutah/forge-forum/internal/slackx"
)

const (
	testTeam    = "T0FORGE"
	testChannel = "C1"
	testRootTS  = "1760950800.000100"
	testBaseURL = "https://forum.test"
)

var testNow = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

// moveFixture is a Mover over a real database with Slack and the classifier faked.
type moveFixture struct {
	mover *Mover
	slack *slackx.Fake
	cls   *FakeClassifier
	store *forum.Store
	db    *db.DB
	wakes int
}

// newMoveFixture returns a fixture whose Slack workspace has one public
// channel, a private channel, a DM, a group DM, and a handful of members.
func newMoveFixture(t *testing.T) *moveFixture {
	t.Helper()
	database, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "forum.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	f := &moveFixture{
		db:    database,
		store: forum.NewStore(database, func() time.Time { return testNow }),
		cls:   &FakeClassifier{Direct: map[string]bool{}},
		slack: &slackx.Fake{
			Threads: map[string][]slackx.Message{},
			Channels: map[string]slackx.Channel{
				testChannel: {ID: testChannel, Name: "kubernetes", IsPublic: true},
				"G1":        {ID: "G1", Name: "board", IsPublic: false},
				"D1":        {ID: "D1", IsPublic: false},
				"MP1":       {ID: "MP1", Name: "mpdm-ada--mo--rae-1", IsPublic: false},
			},
			Users: map[string]slackx.User{
				"UASKER": {ID: "UASKER", Name: "Ada Asker", AvatarURL: "https://avatars.slack-edge.com/ada.png"},
				"UMOVER": {ID: "UMOVER", Name: "Mo Mover"},
				"UOTHER": {ID: "UOTHER", Name: "Oz Other"},
				"UAPP":   {ID: "UAPP", Name: "Deploy Bot", IsBot: true},
			},
		},
	}
	for i := range 5 {
		id := fmt.Sprintf("UR%d", i)
		f.slack.Users[id] = slackx.User{ID: id, Name: fmt.Sprintf("Replier %d", i)}
	}
	f.mover = New(Deps{
		Slack:      f.slack,
		Store:      f.store,
		DB:         database,
		Classifier: f.cls,
		BaseURL:    testBaseURL + "/",
		TeamID:     testTeam,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:        func() time.Time { return testNow },
		Wake:       func() { f.wakes++ },
	})
	return f
}

// replyTS is the Slack timestamp of the i-th reply (1-based) of the seeded thread.
func replyTS(i int) string { return fmt.Sprintf("%d.%06d", 1760950800+60*i, 100+i) }

// seedThread scripts a thread in the public channel: one original post by
// UASKER and n replies from five members in rotation.
func (f *moveFixture) seedThread(n int) {
	msgs := []slackx.Message{{User: "UASKER", TS: testRootTS, ThreadTS: testRootTS,
		Text: "Has anyone moved from Ingress to *Gateway API*?\nWe run 40 clusters &amp; want out."}}
	for i := 1; i <= n; i++ {
		msgs = append(msgs, slackx.Message{
			User: fmt.Sprintf("UR%d", i%5), TS: replyTS(i), ThreadTS: testRootTS, Text: fmt.Sprintf("reply %d", i),
		})
	}
	f.slack.Threads[slackx.ThreadKey(testChannel, testRootTS)] = msgs
}

// shortcut is UMOVER picking "Move to forum" on the seeded thread's original post.
func rootShortcut() slackx.ShortcutEvent {
	return slackx.ShortcutEvent{
		TriggerID: "trigger-1", TeamID: testTeam, UserID: "UMOVER", ChannelID: testChannel,
		MessageTS: testRootTS, MessageUserID: "UASKER",
		MessageText: "Has anyone moved from Ingress to *Gateway API*?\nWe run 40 clusters &amp; want out.",
	}
}

// submission is UMOVER confirming the dialog for the seeded thread.
func rootSubmission() slackx.SubmissionEvent {
	return slackx.SubmissionEvent{
		TeamID: testTeam, UserID: "UMOVER", UserName: "mo",
		Title: "Moving from Ingress to Gateway API", Tags: []string{"kubernetes"},
		PrivateMetadata: fmt.Sprintf(`{"channel":%q,"ts":%q}`, testChannel, testRootTS),
	}
}

// submit sends a submission that must pass validation and waits for its job.
func (f *moveFixture) submit(t *testing.T, sub slackx.SubmissionEvent) {
	t.Helper()
	if errs := f.mover.HandleSubmission(context.Background(), sub); errs != nil {
		t.Fatalf("HandleSubmission returned field errors %v", errs)
	}
	f.mover.Wait()
}

func (f *moveFixture) count(t *testing.T, table string) int {
	t.Helper()
	var n int
	if err := f.db.Reader().QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// outboxRow is one row of the notifications table.
type outboxRow struct {
	Kind string
	Msg  notify.Message
}

func (f *moveFixture) outbox(t *testing.T) []outboxRow {
	t.Helper()
	rows, err := f.db.Reader().Query(`SELECT kind, payload FROM notifications ORDER BY id`)
	if err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	defer rows.Close()
	var out []outboxRow
	for rows.Next() {
		var r outboxRow
		var payload string
		if err := rows.Scan(&r.Kind, &payload); err != nil {
			t.Fatalf("scan outbox: %v", err)
		}
		if err := json.Unmarshal([]byte(payload), &r.Msg); err != nil {
			t.Fatalf("decode outbox payload: %v", err)
		}
		out = append(out, r)
	}
	return out
}

// importedReply is one row of the replies table with its author.
type importedReply struct {
	AuthorSlackID, AuthorName, Body, SlackTS string
	CreatedAt                                int64
	Imported, FirstLevel                     bool
}

func (f *moveFixture) replies(t *testing.T, threadID int64) []importedReply {
	t.Helper()
	rows, err := f.db.Reader().Query(`
		SELECT m.slack_user_id, m.name, r.body_md, COALESCE(r.slack_ts, ''), r.created_at, r.imported, r.parent_id IS NULL
		FROM replies r JOIN members m ON m.id = r.author_id WHERE r.thread_id = ? ORDER BY r.id`, threadID)
	if err != nil {
		t.Fatalf("read replies: %v", err)
	}
	defer rows.Close()
	var out []importedReply
	for rows.Next() {
		var r importedReply
		if err := rows.Scan(&r.AuthorSlackID, &r.AuthorName, &r.Body, &r.SlackTS, &r.CreatedAt, &r.Imported, &r.FirstLevel); err != nil {
			t.Fatalf("scan reply: %v", err)
		}
		out = append(out, r)
	}
	return out
}

// onlyThread returns the single forum thread, failing when there is not exactly one.
func (f *moveFixture) onlyThread(t *testing.T) forum.Thread {
	t.Helper()
	if n := f.count(t, "threads"); n != 1 {
		t.Fatalf("threads = %d, want 1", n)
	}
	var id int64
	if err := f.db.Reader().QueryRow(`SELECT id FROM threads`).Scan(&id); err != nil {
		t.Fatalf("thread id: %v", err)
	}
	th, err := f.store.GetThread(context.Background(), id, 0)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	return th
}

// called reports how many times the fake Slack saw method.
func (f *moveFixture) called(method string) int {
	n := 0
	for _, c := range f.slack.Calls {
		if c.Method == method {
			n++
		}
	}
	return n
}

// requireNothingMoved fails when a refused or failed move left anything behind.
func (f *moveFixture) requireNothingMoved(t *testing.T) {
	t.Helper()
	for _, table := range []string{"threads", "replies", "members"} {
		if n := f.count(t, table); n != 0 {
			t.Errorf("%s = %d rows, want 0", table, n)
		}
	}
}

// moveAE5 runs the acceptance move: 50 replies, the 12 at every fourth position marked direct.
func (f *moveFixture) moveAE5(t *testing.T) (directTS []string) {
	t.Helper()
	f.seedThread(50)
	for i := 4; i <= 48; i += 4 {
		f.cls.Direct[replyTS(i)] = true
		directTS = append(directTS, replyTS(i))
	}
	if err := f.mover.HandleShortcut(context.Background(), rootShortcut()); err != nil {
		t.Fatalf("HandleShortcut: %v", err)
	}
	if len(f.slack.Dialogs) != 1 {
		t.Fatalf("dialogs opened = %d, want 1", len(f.slack.Dialogs))
	}
	sub := rootSubmission()
	sub.PrivateMetadata = f.slack.Dialogs[0].Dialog.PrivateMetadata
	f.submit(t, sub)
	return directTS
}

// TestMover_Move_AE5_ImportsDirectRepliesAndCountsTheRest covers AE5: 12 of 50 replies move, in order, under their authors.
func TestMover_Move_AE5_ImportsDirectRepliesAndCountsTheRest(t *testing.T) {
	f := newMoveFixture(t)
	directTS := f.moveAE5(t)

	th := f.onlyThread(t)
	if th.Title != "Moving from Ingress to Gateway API" || !reflect.DeepEqual(th.Tags, []string{"kubernetes"}) {
		t.Errorf("title = %q, tags = %v", th.Title, th.Tags)
	}
	if want := "Has anyone moved from Ingress to **Gateway API**?\nWe run 40 clusters & want out."; th.BodyMD != want {
		t.Errorf("body = %q\nwant the converted original post %q", th.BodyMD, want)
	}
	if th.AuthorName != "Ada Asker" || th.AuthorAvatar != "https://avatars.slack-edge.com/ada.png" {
		t.Errorf("author = %q (%q), want the original poster", th.AuthorName, th.AuthorAvatar)
	}
	if !th.FromSlack || th.SlackRemaining != 38 || th.SlackChannelName != "kubernetes" {
		t.Errorf("FromSlack = %v, SlackRemaining = %d, channel = %q; want true, 38, kubernetes", th.FromSlack, th.SlackRemaining, th.SlackChannelName)
	}
	if want := "https://slack.test/archives/C1/p1760950800000100"; th.SlackPermalink != want {
		t.Errorf("permalink = %q, want %q", th.SlackPermalink, want)
	}
	if th.ReplyCount != 12 || th.CreatedAt.Unix() != 1760950800 || !th.LastActivityAt.Equal(testNow) {
		t.Errorf("ReplyCount = %d, CreatedAt = %d, LastActivityAt = %v", th.ReplyCount, th.CreatedAt.Unix(), th.LastActivityAt)
	}

	replies := f.replies(t, th.ID)
	if len(replies) != 12 {
		t.Fatalf("imported replies = %d, want 12", len(replies))
	}
	for i, r := range replies {
		n := 4 * (i + 1)
		want := importedReply{
			AuthorSlackID: fmt.Sprintf("UR%d", n%5), AuthorName: fmt.Sprintf("Replier %d", n%5),
			Body: fmt.Sprintf("reply %d", n), SlackTS: directTS[i], CreatedAt: int64(1760950800 + 60*n),
			Imported: true, FirstLevel: true,
		}
		if r != want {
			t.Errorf("reply %d = %+v\nwant %+v", i, r, want)
		}
	}
	if f.cls.Calls != 1 {
		t.Errorf("classifier calls = %d, want 1", f.cls.Calls)
	}
	if f.wakes != 1 {
		t.Errorf("outbox wakes = %d, want 1", f.wakes)
	}
}

// TestMover_Move_AE6_LinkBackOnlyNoAnnouncementNoDMs covers AE6: the only notification is the link into the Slack thread.
func TestMover_Move_AE6_LinkBackOnlyNoAnnouncementNoDMs(t *testing.T) {
	f := newMoveFixture(t)
	f.moveAE5(t)
	th := f.onlyThread(t)

	rows := f.outbox(t)
	if len(rows) != 1 {
		t.Fatalf("outbox = %+v, want exactly one row", rows)
	}
	got := rows[0]
	if got.Kind != string(notify.KindLinkBack) || got.Msg.Channel != testChannel || got.Msg.ThreadTS != testRootTS {
		t.Errorf("outbox row = %+v, want a link_back into the source Slack thread", got)
	}
	wantText := notify.LinkBackText("UMOVER", 12, 38, th.Title, fmt.Sprintf("%s/threads/%d", testBaseURL, th.ID))
	if got.Msg.Text != wantText {
		t.Errorf("link-back text = %q\nwant %q", got.Msg.Text, wantText)
	}
	if posted := f.slack.PostedMessages(); len(posted) != 0 {
		t.Errorf("the move posted to Slack directly: %+v", posted)
	}
}

// TestMover_HandleShortcut_AE7_AlreadyMovedGivesExistingLink covers AE7 at the shortcut: no dialog, the member gets the link.
func TestMover_HandleShortcut_AE7_AlreadyMovedGivesExistingLink(t *testing.T) {
	f := newMoveFixture(t)
	f.moveAE5(t)
	th := f.onlyThread(t)
	link := fmt.Sprintf("%s/threads/%d", testBaseURL, th.ID)

	onRoot := rootShortcut()
	onRoot.UserID = "UOTHER"
	onReply := onRoot
	onReply.MessageTS, onReply.ThreadTS, onReply.MessageUserID = replyTS(3), testRootTS, "UR3"

	for name, sc := range map[string]slackx.ShortcutEvent{"on the original post": onRoot, "on a reply": onReply} {
		t.Run(name, func(t *testing.T) {
			before := len(f.slack.PostedMessages())
			if err := f.mover.HandleShortcut(context.Background(), sc); err != nil {
				t.Fatalf("HandleShortcut: %v", err)
			}
			if len(f.slack.Dialogs) != 1 {
				t.Errorf("dialogs opened = %d, want only the first move's", len(f.slack.Dialogs))
			}
			posted := f.slack.PostedMessages()[before:]
			if len(posted) != 1 || posted[0].Channel != "UOTHER" || !strings.Contains(posted[0].Text, link) {
				t.Errorf("posted = %+v, want one direct message to UOTHER carrying %s", posted, link)
			}
		})
	}
	if th, r, n := f.count(t, "threads"), f.count(t, "replies"), f.count(t, "notifications"); th != 1 || r != 12 || n != 1 {
		t.Errorf("threads=%d replies=%d notifications=%d, want 1, 12, 1", th, r, n)
	}
}

// TestMover_HandleSubmission_AE7_SecondJobCreatesNothing covers AE7 at the job: a repeated submission imports nothing.
func TestMover_HandleSubmission_AE7_SecondJobCreatesNothing(t *testing.T) {
	f := newMoveFixture(t)
	f.moveAE5(t)
	th := f.onlyThread(t)

	again := rootSubmission()
	again.UserID, again.Title = "UOTHER", "Another go at the same thread"
	f.submit(t, again)

	if th, r, n := f.count(t, "threads"), f.count(t, "replies"), f.count(t, "notifications"); th != 1 || r != 12 || n != 1 {
		t.Errorf("threads=%d replies=%d notifications=%d, want 1, 12, 1", th, r, n)
	}
	if f.cls.Calls != 1 {
		t.Errorf("classifier calls = %d, want 1: the second job must stop before classifying", f.cls.Calls)
	}
	posted := f.slack.PostedMessages()
	link := fmt.Sprintf("%s/threads/%d", testBaseURL, th.ID)
	if len(posted) != 1 || posted[0].Channel != "UOTHER" || !strings.Contains(posted[0].Text, link) {
		t.Errorf("posted = %+v, want one direct message to UOTHER carrying %s", posted, link)
	}
}

// blockingClassifier holds Classify until released, to keep a job in flight.
type blockingClassifier struct {
	entered chan struct{}
	release chan struct{}
	calls   int
}

func (c *blockingClassifier) Classify(context.Context, SlackMessage, []SlackMessage) (map[string]bool, error) {
	c.calls++
	c.entered <- struct{}{}
	<-c.release
	return map[string]bool{}, nil
}

// TestMover_HandleSubmission_RedeliveryWhileMovingStartsOneJob checks a submission Slack redelivers mid-move is dropped.
func TestMover_HandleSubmission_RedeliveryWhileMovingStartsOneJob(t *testing.T) {
	f := newMoveFixture(t)
	f.seedThread(3)
	cls := &blockingClassifier{entered: make(chan struct{}, 2), release: make(chan struct{})}
	f.mover.d.Classifier = cls

	if errs := f.mover.HandleSubmission(context.Background(), rootSubmission()); errs != nil {
		t.Fatalf("first submission: %v", errs)
	}
	<-cls.entered
	if errs := f.mover.HandleSubmission(context.Background(), rootSubmission()); errs != nil {
		t.Fatalf("redelivered submission: %v", errs)
	}
	close(cls.release)
	f.mover.Wait()

	if cls.calls != 1 {
		t.Errorf("classifier calls = %d, want 1", cls.calls)
	}
	if th, n := f.count(t, "threads"), f.count(t, "notifications"); th != 1 || n != 1 {
		t.Errorf("threads=%d notifications=%d, want 1 and 1", th, n)
	}
	if posted := f.slack.PostedMessages(); len(posted) != 0 {
		t.Errorf("posted = %+v, want nothing", posted)
	}
}

// TestMover_HandleShortcut_AE8_NonPublicChannelRefused covers AE8: no dialog opens outside a public channel.
func TestMover_HandleShortcut_AE8_NonPublicChannelRefused(t *testing.T) {
	channels := map[string]string{
		"private channel":                  "G1",
		"direct message":                   "D1",
		"group direct message":             "MP1",
		"conversation the bot cannot read": "D404",
	}
	for name, channel := range channels {
		t.Run(name, func(t *testing.T) {
			f := newMoveFixture(t)
			sc := rootShortcut()
			sc.ChannelID = channel
			if err := f.mover.HandleShortcut(context.Background(), sc); err != nil {
				t.Fatalf("HandleShortcut: %v", err)
			}
			if len(f.slack.Dialogs) != 0 || f.called("OpenMoveDialog") != 0 {
				t.Errorf("a dialog opened for %s", name)
			}
			posted := f.slack.PostedMessages()
			if len(posted) != 1 || posted[0].Channel != "UMOVER" || posted[0].ThreadTS != "" ||
				!strings.Contains(posted[0].Text, "public channels") {
				t.Errorf("posted = %+v, want one direct message to the member saying why", posted)
			}
			f.requireNothingMoved(t)
		})
	}
}

// TestMover_HandleSubmission_NonPublicChannelRefusedByJob checks the job repeats the public-channel check.
func TestMover_HandleSubmission_NonPublicChannelRefusedByJob(t *testing.T) {
	f := newMoveFixture(t)
	sub := rootSubmission()
	sub.PrivateMetadata = `{"channel":"G1","ts":"1760950800.000100"}`
	f.submit(t, sub)

	f.requireNothingMoved(t)
	if f.called("ThreadReplies") != 0 {
		t.Error("the job read a thread in a private channel")
	}
	posted := f.slack.PostedMessages()
	if len(posted) != 1 || posted[0].Channel != "UMOVER" || !strings.Contains(posted[0].Text, "public channels") {
		t.Errorf("posted = %+v, want one direct message to the member saying why", posted)
	}
}

// TestMover_Move_CreditsAuthorsWhoNeverSignedIn checks unseen Slack authors and the mover get member rows by Slack name.
func TestMover_Move_CreditsAuthorsWhoNeverSignedIn(t *testing.T) {
	f := newMoveFixture(t)
	f.seedThread(2)
	// One replier's profile cannot be read; they are credited by Slack ID.
	f.slack.Threads[slackx.ThreadKey(testChannel, testRootTS)][2].User = "UGONE"
	f.cls.Direct[replyTS(1)], f.cls.Direct[replyTS(2)] = true, true
	f.submit(t, rootSubmission())

	rows, err := f.db.Reader().Query(`SELECT slack_user_id, name, forge_user_id IS NULL FROM members ORDER BY slack_user_id`)
	if err != nil {
		t.Fatalf("members: %v", err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var slackID, name string
		var noForgeUser bool
		if err := rows.Scan(&slackID, &name, &noForgeUser); err != nil {
			t.Fatalf("scan member: %v", err)
		}
		if !noForgeUser {
			t.Errorf("member %s has a Forge user ID, want none", slackID)
		}
		got[slackID] = name
	}
	want := map[string]string{"UASKER": "Ada Asker", "UMOVER": "Mo Mover", "UR1": "Replier 1", "UGONE": "UGONE"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("members = %v\nwant %v", got, want)
	}

	// The row is keyed on the Slack user ID, so it is the one the author
	// gets when they first sign in, and the post is theirs to edit.
	th := f.onlyThread(t)
	var signedIn int64
	err = f.db.WithWriteTx(context.Background(), func(tx *sql.Tx) error {
		var err error
		signedIn, err = identity.EnsureBySlackID(context.Background(), tx, "UASKER", "Ada", "")
		return err
	})
	if err != nil || signedIn != th.AuthorID {
		t.Errorf("member for UASKER = %d, %v; want the thread's author %d", signedIn, err, th.AuthorID)
	}
	var movedBy string
	if err := f.db.Reader().QueryRow(`SELECT m.slack_user_id FROM threads t JOIN members m ON m.id = t.moved_by`).Scan(&movedBy); err != nil || movedBy != "UMOVER" {
		t.Errorf("moved_by = %q, %v; want UMOVER", movedBy, err)
	}
}

// TestMover_Move_ShortcutOnReplyMovesWholeThread checks a shortcut on a reply moves the thread from its root.
func TestMover_Move_ShortcutOnReplyMovesWholeThread(t *testing.T) {
	f := newMoveFixture(t)
	f.seedThread(5)
	f.cls.Direct[replyTS(2)] = true

	sc := rootShortcut()
	sc.MessageTS, sc.ThreadTS, sc.MessageUserID, sc.MessageText = replyTS(4), testRootTS, "UR4", "reply 4"
	if err := f.mover.HandleShortcut(context.Background(), sc); err != nil {
		t.Fatalf("HandleShortcut: %v", err)
	}
	if len(f.slack.Dialogs) != 1 {
		t.Fatalf("dialogs opened = %d, want 1", len(f.slack.Dialogs))
	}
	dialog := f.slack.Dialogs[0].Dialog
	if dialog.Title != "Has anyone moved from Ingress to Gateway API?" {
		t.Errorf("pre-filled title = %q, want the original post's first line", dialog.Title)
	}
	var meta moveMetadata
	if err := json.Unmarshal([]byte(dialog.PrivateMetadata), &meta); err != nil || meta.Channel != testChannel || meta.TS != testRootTS {
		t.Errorf("metadata = %q, want the channel and the thread root", dialog.PrivateMetadata)
	}

	sub := rootSubmission()
	sub.PrivateMetadata = dialog.PrivateMetadata
	f.submit(t, sub)

	th := f.onlyThread(t)
	if !strings.HasPrefix(th.BodyMD, "Has anyone moved") || th.AuthorName != "Ada Asker" || th.SlackRemaining != 4 {
		t.Errorf("thread = %q by %q, %d remaining; want the original post, Ada Asker, 4", th.BodyMD, th.AuthorName, th.SlackRemaining)
	}
	if replies := f.replies(t, th.ID); len(replies) != 1 || replies[0].SlackTS != replyTS(2) {
		t.Errorf("replies = %+v, want only reply 2", replies)
	}
}

// TestMover_Move_ReplyThatDoesNotNameItsRootStillMovesFromRoot checks the job finds the root when the shortcut payload lacks it.
func TestMover_Move_ReplyThatDoesNotNameItsRootStillMovesFromRoot(t *testing.T) {
	f := newMoveFixture(t)
	f.seedThread(3)
	sub := rootSubmission()
	sub.PrivateMetadata = fmt.Sprintf(`{"channel":%q,"ts":%q}`, testChannel, replyTS(2))
	f.submit(t, sub)

	th := f.onlyThread(t)
	if th.AuthorName != "Ada Asker" || th.SlackRemaining != 3 {
		t.Errorf("thread by %q with %d remaining, want the root post by Ada Asker with 3", th.AuthorName, th.SlackRemaining)
	}
	if rows := f.outbox(t); len(rows) != 1 || rows[0].Msg.ThreadTS != testRootTS {
		t.Errorf("outbox = %+v, want a link-back into the thread root", rows)
	}

	// The same thread reached through another reply is recognised as moved.
	sub.PrivateMetadata = fmt.Sprintf(`{"channel":%q,"ts":%q}`, testChannel, replyTS(3))
	f.submit(t, sub)
	if n := f.count(t, "threads"); n != 1 {
		t.Errorf("threads = %d after moving through a second reply, want 1", n)
	}
	if posted := f.slack.PostedMessages(); len(posted) != 1 || !strings.Contains(posted[0].Text, fmt.Sprintf("/threads/%d", th.ID)) {
		t.Errorf("posted = %+v, want the existing link", posted)
	}
}

// requireMoveFailed fails unless the only trace of a move is a move_failed message to the mover.
func (f *moveFixture) requireMoveFailed(t *testing.T) {
	t.Helper()
	f.requireNothingMoved(t)
	rows := f.outbox(t)
	want := outboxRow{Kind: string(notify.KindMoveFailed), Msg: notify.Message{
		Channel: "UMOVER", Text: notify.MoveFailedText("Moving from Ingress to Gateway API"),
	}}
	if len(rows) != 1 || rows[0] != want {
		t.Errorf("outbox = %+v\nwant only %+v", rows, want)
	}
	if f.wakes != 1 {
		t.Errorf("outbox wakes = %d, want 1", f.wakes)
	}
	if posted := f.slack.PostedMessages(); len(posted) != 0 {
		t.Errorf("posted = %+v, want nothing sent outside the outbox", posted)
	}
}

// TestMover_Move_ClassifierFailureImportsNothingAndTellsMover checks a classification error leaves no thread behind.
func TestMover_Move_ClassifierFailureImportsNothingAndTellsMover(t *testing.T) {
	f := newMoveFixture(t)
	f.seedThread(6)
	f.cls.Err = errors.New("anthropic: overloaded")
	f.submit(t, rootSubmission())
	f.requireMoveFailed(t)
}

// TestMover_Move_ThreadReadFailureImportsNothingAndTellsMover checks a Slack read error leaves no thread behind.
func TestMover_Move_ThreadReadFailureImportsNothingAndTellsMover(t *testing.T) {
	f := newMoveFixture(t) // no thread scripted: Slack answers thread_not_found
	f.submit(t, rootSubmission())
	f.requireMoveFailed(t)
	if f.cls.Calls != 0 {
		t.Errorf("classifier calls = %d, want 0", f.cls.Calls)
	}
}

// TestMover_Move_ImportFailureImportsNothingAndTellsMover checks a tag retired while the job ran fails the move whole.
func TestMover_Move_ImportFailureImportsNothingAndTellsMover(t *testing.T) {
	f := newMoveFixture(t)
	f.seedThread(2)
	f.mover.d.Classifier = classifierFunc(func() (map[string]bool, error) {
		err := f.db.WithWriteTx(context.Background(), func(tx *sql.Tx) error {
			_, err := tx.Exec(`UPDATE tags SET active = 0 WHERE slug = 'kubernetes'`)
			return err
		})
		return map[string]bool{replyTS(1): true}, err
	})
	f.submit(t, rootSubmission())
	f.requireMoveFailed(t)
}

// classifierFunc adapts a function to Classifier.
type classifierFunc func() (map[string]bool, error)

func (fn classifierFunc) Classify(context.Context, SlackMessage, []SlackMessage) (map[string]bool, error) {
	return fn()
}

// TestMover_HandleSubmission_InvalidInputReturnsFieldErrors checks each bad input is reported on its own block and starts no job.
func TestMover_HandleSubmission_InvalidInputReturnsFieldErrors(t *testing.T) {
	cases := []struct {
		name   string
		edit   func(*slackx.SubmissionEvent)
		blocks []string
	}{
		{"no tag", func(s *slackx.SubmissionEvent) { s.Tags = nil }, []string{slackx.MoveTagsBlockID}},
		{"blank tag", func(s *slackx.SubmissionEvent) { s.Tags = []string{" "} }, []string{slackx.MoveTagsBlockID}},
		{"unknown tag", func(s *slackx.SubmissionEvent) { s.Tags = []string{"kubernetes", "nope"} }, []string{slackx.MoveTagsBlockID}},
		{"retired tag", func(s *slackx.SubmissionEvent) { s.Tags = []string{"careers"} }, []string{slackx.MoveTagsBlockID}},
		{"empty title", func(s *slackx.SubmissionEvent) { s.Title = " \t" }, []string{slackx.MoveTitleBlockID}},
		{"title too long", func(s *slackx.SubmissionEvent) { s.Title = strings.Repeat("é", forum.MaxTitleLen+1) }, []string{slackx.MoveTitleBlockID}},
		{"no title and no tag", func(s *slackx.SubmissionEvent) { s.Title, s.Tags = "", nil }, []string{slackx.MoveTagsBlockID, slackx.MoveTitleBlockID}},
		{"unreadable metadata", func(s *slackx.SubmissionEvent) { s.PrivateMetadata = "not json" }, []string{slackx.MoveTitleBlockID}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newMoveFixture(t)
			f.seedThread(3)
			err := f.db.WithWriteTx(context.Background(), func(tx *sql.Tx) error {
				_, err := tx.Exec(`UPDATE tags SET active = 0 WHERE slug = 'careers'`)
				return err
			})
			if err != nil {
				t.Fatalf("retire tag: %v", err)
			}
			sub := rootSubmission()
			tc.edit(&sub)

			errs := f.mover.HandleSubmission(context.Background(), sub)
			f.mover.Wait()

			var blocks []string
			for block, msg := range errs {
				if msg == "" {
					t.Errorf("block %q has an empty message", block)
				}
				blocks = append(blocks, block)
			}
			if len(blocks) == 2 && blocks[0] > blocks[1] {
				blocks[0], blocks[1] = blocks[1], blocks[0]
			}
			if !reflect.DeepEqual(blocks, tc.blocks) {
				t.Errorf("field errors on %v, want %v (%v)", blocks, tc.blocks, errs)
			}
			if len(f.slack.Calls) != 0 || f.cls.Calls != 0 {
				t.Errorf("a job ran: Slack calls %+v, classifier calls %d", f.slack.Calls, f.cls.Calls)
			}
			f.requireNothingMoved(t)
			if n := f.count(t, "notifications"); n != 0 {
				t.Errorf("notifications = %d, want 0", n)
			}
		})
	}
}

// recordingClassifier records the replies it was asked about and marks them all direct.
type recordingClassifier struct {
	original SlackMessage
	replies  []SlackMessage
}

func (c *recordingClassifier) Classify(_ context.Context, original SlackMessage, replies []SlackMessage) (map[string]bool, error) {
	c.original, c.replies = original, replies
	direct := map[string]bool{}
	for _, r := range replies {
		direct[r.ID] = true
	}
	return direct, nil
}

// TestMover_Move_NonMemberRepliesStayInSlack checks bot and system replies are neither classified nor imported, and count as remaining.
func TestMover_Move_NonMemberRepliesStayInSlack(t *testing.T) {
	f := newMoveFixture(t)
	rec := &recordingClassifier{}
	f.mover.d.Classifier = rec
	f.slack.Threads[slackx.ThreadKey(testChannel, testRootTS)] = []slackx.Message{
		{User: "UASKER", TS: testRootTS, ThreadTS: testRootTS, Text: "How do we do this?"},
		{User: "UR1", TS: replyTS(1), Text: "A member's answer"},
		{User: "UAPP", BotID: "B1", TS: replyTS(2), Text: "Deploy finished"},
		{BotID: "B2", SubType: "bot_message", TS: replyTS(3), Text: "Webhook says hi"},
		{User: "UR2", SubType: "channel_join", TS: replyTS(4), Text: "<@UR2> has joined the channel"},
		{TS: replyTS(5), Text: "A message with no author"},
		{User: "UAPP", TS: replyTS(6), Text: "Posted by a bot user without a bot ID"},
		{User: "UR3", TS: replyTS(7), Text: "  "},
		{User: "UR4", SubType: "thread_broadcast", TS: replyTS(8), Text: "Also sent to the channel"},
		{User: "UR0", SubType: "file_share", TS: replyTS(9), Text: "See the screenshot"},
	}
	f.submit(t, rootSubmission())

	var classified []string
	for _, r := range rec.replies {
		classified = append(classified, r.ID)
	}
	wantTS := []string{replyTS(1), replyTS(8), replyTS(9)}
	if !reflect.DeepEqual(classified, wantTS) {
		t.Errorf("classified %v\nwant only the member replies %v", classified, wantTS)
	}
	if rec.original.ID != testRootTS || rec.original.Author != "Ada Asker" || rec.replies[0].Author != "Replier 1" {
		t.Errorf("classifier saw original %+v and first reply %+v", rec.original, rec.replies[0])
	}

	th := f.onlyThread(t)
	var imported []string
	for _, r := range f.replies(t, th.ID) {
		imported = append(imported, r.SlackTS)
	}
	if !reflect.DeepEqual(imported, wantTS) {
		t.Errorf("imported %v, want %v", imported, wantTS)
	}
	if th.SlackRemaining != 6 {
		t.Errorf("SlackRemaining = %d, want 6", th.SlackRemaining)
	}
	if n := f.count(t, "members"); n != 5 {
		t.Errorf("members = %d, want 5: no row for a bot or an author whose reply stayed in Slack", n)
	}
}

// TestMover_Move_ThreadWithNoMemberRepliesSkipsClassifier checks a thread with nothing to classify still moves.
func TestMover_Move_ThreadWithNoMemberRepliesSkipsClassifier(t *testing.T) {
	f := newMoveFixture(t)
	f.slack.Threads[slackx.ThreadKey(testChannel, testRootTS)] = []slackx.Message{
		{User: "UASKER", TS: testRootTS, Text: "Anyone?"},
		{User: "UAPP", BotID: "B1", TS: replyTS(1), Text: "Deploy finished"},
	}
	f.submit(t, rootSubmission())

	th := f.onlyThread(t)
	if f.cls.Calls != 0 || th.ReplyCount != 0 || th.SlackRemaining != 1 {
		t.Errorf("classifier calls = %d, ReplyCount = %d, SlackRemaining = %d; want 0, 0, 1", f.cls.Calls, th.ReplyCount, th.SlackRemaining)
	}
}

// TestMover_Move_ThreadStartedByAppRefused checks a thread whose original post has no human author is not moved.
func TestMover_Move_ThreadStartedByAppRefused(t *testing.T) {
	t.Run("at the shortcut", func(t *testing.T) {
		f := newMoveFixture(t)
		sc := rootShortcut()
		sc.MessageUserID, sc.MessageBotID = "UAPP", "B1"
		if err := f.mover.HandleShortcut(context.Background(), sc); err != nil {
			t.Fatalf("HandleShortcut: %v", err)
		}
		posted := f.slack.PostedMessages()
		if len(f.slack.Dialogs) != 0 || len(posted) != 1 || posted[0].Channel != "UMOVER" || posted[0].Text != notMemberText {
			t.Errorf("dialogs = %d, posted = %+v; want no dialog and the refusal sent to the member", len(f.slack.Dialogs), posted)
		}
	})
	t.Run("at the shortcut on a reply", func(t *testing.T) {
		f := newMoveFixture(t)
		f.seedThread(2)
		f.slack.Threads[slackx.ThreadKey(testChannel, testRootTS)][0] = slackx.Message{BotID: "B1", SubType: "bot_message", TS: testRootTS, Text: "Alert"}
		sc := rootShortcut()
		sc.MessageTS, sc.ThreadTS, sc.MessageUserID = replyTS(1), testRootTS, "UR1"
		if err := f.mover.HandleShortcut(context.Background(), sc); err != nil {
			t.Fatalf("HandleShortcut: %v", err)
		}
		if posted := f.slack.PostedMessages(); len(f.slack.Dialogs) != 0 || len(posted) != 1 || posted[0].Text != notMemberText {
			t.Errorf("dialogs = %d, posted = %+v; want no dialog and the refusal", len(f.slack.Dialogs), posted)
		}
	})
	t.Run("in the job", func(t *testing.T) {
		f := newMoveFixture(t)
		f.slack.Threads[slackx.ThreadKey(testChannel, testRootTS)] = []slackx.Message{
			{User: "UAPP", TS: testRootTS, Text: "Posted by a bot user"},
			{User: "UR1", TS: replyTS(1), Text: "A reply"},
		}
		f.submit(t, rootSubmission())
		f.requireNothingMoved(t)
		posted := f.slack.PostedMessages()
		if len(posted) != 1 || posted[0].Channel != "UMOVER" || posted[0].Text != notMemberText || f.count(t, "notifications") != 0 {
			t.Errorf("posted = %+v, want only the refusal sent to the member", posted)
		}
	})
}

// TestMover_Move_MentionsResolveToDisplayNames checks mentions in the post and replies become names.
func TestMover_Move_MentionsResolveToDisplayNames(t *testing.T) {
	f := newMoveFixture(t)
	f.slack.Threads[slackx.ThreadKey(testChannel, testRootTS)] = []slackx.Message{
		{User: "UASKER", TS: testRootTS, Text: "<@UR1> and <@UNOBODY> should see this"},
		{User: "UR2", TS: replyTS(1), Text: "cc <@UASKER>, see <https://gateway-api.sigs.k8s.io|the docs>"},
	}
	f.cls.Direct[replyTS(1)] = true
	f.submit(t, rootSubmission())

	th := f.onlyThread(t)
	if want := "@Replier 1 and @unknown should see this"; th.BodyMD != want {
		t.Errorf("body = %q, want %q", th.BodyMD, want)
	}
	replies := f.replies(t, th.ID)
	if want := "cc @Ada Asker, see [the docs](https://gateway-api.sigs.k8s.io)"; len(replies) != 1 || replies[0].Body != want {
		t.Errorf("replies = %+v, want one with body %q", replies, want)
	}
	if n := f.called("UserInfo"); n != 5 {
		t.Errorf("UserInfo calls = %d, want 5: each user looked up once", n)
	}
	if n := f.count(t, "members"); n != 3 {
		t.Errorf("members = %d, want 3: a mention creates no member", n)
	}
}

// TestMover_OtherWorkspaceIgnored checks interactions from another Slack team do nothing.
func TestMover_OtherWorkspaceIgnored(t *testing.T) {
	f := newMoveFixture(t)
	f.seedThread(3)

	sc := rootShortcut()
	sc.TeamID = "T0OTHER"
	if err := f.mover.HandleShortcut(context.Background(), sc); err != nil {
		t.Fatalf("HandleShortcut: %v", err)
	}
	sub := rootSubmission()
	sub.TeamID = "T0OTHER"
	if errs := f.mover.HandleSubmission(context.Background(), sub); errs != nil {
		t.Errorf("HandleSubmission = %v, want nil", errs)
	}
	f.mover.Wait()

	if len(f.slack.Calls) != 0 {
		t.Errorf("Slack calls = %+v, want none", f.slack.Calls)
	}
	f.requireNothingMoved(t)
}

// TestMover_HandleShortcut_OpensDialogWithTitleTagsAndThread checks what the dialog is opened with.
func TestMover_HandleShortcut_OpensDialogWithTitleTagsAndThread(t *testing.T) {
	f := newMoveFixture(t)
	if err := f.mover.HandleShortcut(context.Background(), rootShortcut()); err != nil {
		t.Fatalf("HandleShortcut: %v", err)
	}
	if len(f.slack.Dialogs) != 1 {
		t.Fatalf("dialogs opened = %d, want 1", len(f.slack.Dialogs))
	}
	got := f.slack.Dialogs[0]
	active, err := f.store.ActiveTags(context.Background())
	if err != nil {
		t.Fatalf("ActiveTags: %v", err)
	}
	want := slackx.OpenedDialog{TriggerID: "trigger-1", Dialog: slackx.MoveDialog{
		Title:           "Has anyone moved from Ingress to Gateway API?",
		Tags:            active,
		PrivateMetadata: `{"channel":"C1","ts":"1760950800.000100"}`,
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("dialog = %+v\nwant %+v", got, want)
	}
	// The trigger lasts three seconds: a shortcut on the original post
	// must not read the thread first.
	if f.called("ThreadReplies") != 0 || f.called("UserInfo") != 0 {
		t.Errorf("Slack calls before the dialog: %+v", f.slack.Calls)
	}
	if posted := f.slack.PostedMessages(); len(posted) != 0 {
		t.Errorf("posted = %+v, want nothing", posted)
	}
}

// TestMover_HandleShortcut_DialogFailureIsReturned checks a dialog Slack refuses to open surfaces as an error.
func TestMover_HandleShortcut_DialogFailureIsReturned(t *testing.T) {
	f := newMoveFixture(t)
	f.slack.DialogErr = &slackx.APIError{Code: "expired_trigger_id"}
	if err := f.mover.HandleShortcut(context.Background(), rootShortcut()); slackx.ErrorCode(err) != "expired_trigger_id" {
		t.Errorf("HandleShortcut = %v, want expired_trigger_id", err)
	}
}

// TestTitleFromPost checks the suggested title is the first line, as plain text, within the limit.
func TestTitleFromPost(t *testing.T) {
	f := newMoveFixture(t)
	long := strings.Repeat("wörd ", 60) // 300 characters
	cases := []struct{ name, text, want string }{
		{"first line only", "How do we deploy this?\nSecond line with detail.", "How do we deploy this?"},
		{"leading blank lines", "\n  \nThe real first line\nmore", "The real first line"},
		{"emphasis stripped", "*Bold* and _italic_ and ~struck~ and `code` here", "Bold and italic and struck and code here"},
		{"adjacent emphasis", "*one* *two* _three_", "one two three"},
		{"markers inside words kept", "set max_open_conns to 2*3*4 for my_var", "set max_open_conns to 2*3*4 for my_var"},
		{"entities unescaped", "Ingress &amp; Gateway: a &lt;b&gt; b?", "Ingress & Gateway: a <b> b?"},
		{"mention resolved", "<@UASKER> asked about this", "@Ada Asker asked about this"},
		{"unknown mention falls back to its label", "<@UNOBODY|sam> and <@UNOBODY> asked", "sam and asked"},
		{"channel and link labels", "See <#C1|kubernetes> and <https://example.com/a|the docs> or <https://example.com/b>", "See #kubernetes and the docs or https://example.com/b"},
		{"broadcast", "<!here> lunch?", "@here lunch?"},
		{"quote marker", "&gt; quoted question?\nanswer", "quoted question?"},
		{"bullet", "• first item\n• second", "first item"},
		{"words starting with marker letters", "try this: get the logs", "try this: get the logs"},
		{"whitespace collapsed", "  spaced \t out   title  ", "spaced out title"},
		{"capped", long, strings.TrimSpace(string([]rune(long)[:slackx.MoveTitleMaxLen]))},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := titleFromPost(tc.text, newUserCache(context.Background(), f.slack))
			if got != tc.want {
				t.Errorf("titleFromPost(%q) = %q, want %q", tc.text, got, tc.want)
			}
			if n := len([]rune(got)); n > slackx.MoveTitleMaxLen {
				t.Errorf("title is %d characters, over the limit of %d", n, slackx.MoveTitleMaxLen)
			}
		})
	}
}

// TestMover_HandleShortcut_UnreadableThreadStillOpensDialog checks a failed thread read costs the title, not the dialog.
func TestMover_HandleShortcut_UnreadableThreadStillOpensDialog(t *testing.T) {
	f := newMoveFixture(t) // no thread scripted: Slack answers thread_not_found
	sc := rootShortcut()
	sc.MessageTS, sc.ThreadTS, sc.MessageUserID, sc.MessageText = replyTS(1), testRootTS, "UR1", "reply 1"
	if err := f.mover.HandleShortcut(context.Background(), sc); err != nil {
		t.Fatalf("HandleShortcut: %v", err)
	}
	if len(f.slack.Dialogs) != 1 {
		t.Fatalf("dialogs opened = %d, want 1", len(f.slack.Dialogs))
	}
	if d := f.slack.Dialogs[0].Dialog; d.Title != "" || d.PrivateMetadata != `{"channel":"C1","ts":"1760950800.000100"}` {
		t.Errorf("dialog = %+v, want an empty title for the thread root", d)
	}
}
