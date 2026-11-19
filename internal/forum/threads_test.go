package forum

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/forgeutah/forge-forum/internal/db"
	"github.com/forgeutah/forge-forum/internal/identity"
)

// fakeClock hands out times one minute apart so every post has a distinct,
// ordered timestamp.
type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time {
	c.t = c.t.Add(time.Minute)
	return c.t
}

func newTestStore(t *testing.T) (*Store, *db.DB) {
	t.Helper()
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "forum.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	clock := &fakeClock{t: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)}
	return NewStore(d, clock.Now), d
}

func addMember(t *testing.T, d *db.DB, slackID, name string) int64 {
	t.Helper()
	var id int64
	err := d.WithWriteTx(context.Background(), func(tx *sql.Tx) error {
		var err error
		id, err = identity.EnsureBySlackID(context.Background(), tx, slackID, name, "", time.Unix(1_700_000_000, 0))
		return err
	})
	if err != nil {
		t.Fatalf("add member: %v", err)
	}
	return id
}

func mustCreate(t *testing.T, s *Store, author int64, title string, tags ...string) int64 {
	t.Helper()
	id, err := s.CreateThread(context.Background(), author, title, "Body of "+title, tags)
	if err != nil {
		t.Fatalf("CreateThread(%q): %v", title, err)
	}
	return id
}

func exec(t *testing.T, d *db.DB, query string, args ...any) {
	t.Helper()
	err := d.WithWriteTx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(query, args...)
		return err
	})
	if err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

func titles(threads []Thread) []string {
	out := make([]string, len(threads))
	for i, th := range threads {
		out[i] = th.Title
	}
	return out
}

func mustList(t *testing.T, s *Store, opts ListOptions) ([]Thread, bool) {
	t.Helper()
	threads, more, err := s.ListThreads(context.Background(), opts)
	if err != nil {
		t.Fatalf("ListThreads(%+v): %v", opts, err)
	}
	return threads, more
}

func countRows(t *testing.T, d *db.DB, table string) int {
	t.Helper()
	var n int
	if err := d.Reader().QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// TestStore_CreateThread_TitleBodyTwoTags_TopOfFeedWithBothTags checks the
// main path: a new thread leads the feed and carries its tags and author.
func TestStore_CreateThread_TitleBodyTwoTags_TopOfFeedWithBothTags(t *testing.T) {
	s, d := newTestStore(t)
	sam := addMember(t, d, "U0SAM", "Sam R")
	mustCreate(t, s, sam, "Older thread", "general")
	id, err := s.CreateThread(context.Background(), sam, "  Who is building software factories?  ", "Is anyone here running one?", []string{"ai", "architecture"})
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	threads, more := mustList(t, s, ListOptions{})
	if more || len(threads) != 2 {
		t.Fatalf("feed = %v more = %v, want 2 threads and no more", titles(threads), more)
	}
	top := threads[0]
	if top.ID != id || top.Title != "Who is building software factories?" {
		t.Errorf("top of feed = %d %q", top.ID, top.Title)
	}
	if !slices.Equal(top.Tags, []string{"ai", "architecture"}) {
		t.Errorf("tags = %v, want [ai architecture]", top.Tags)
	}
	if top.AuthorID != sam || top.AuthorName != "Sam R" || top.Score != 0 || top.ReplyCount != 0 {
		t.Errorf("thread = %+v", top)
	}
	if top.CreatedAt.IsZero() || !top.CreatedAt.Equal(top.LastActivityAt) || top.EditedAt != nil || top.DeletedAt != nil || top.FromSlack {
		t.Errorf("timestamps or flags wrong: %+v", top)
	}

	got, err := s.GetThread(context.Background(), id, sam)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if got.BodyMD != "Is anyone here running one?" || !slices.Equal(got.Tags, top.Tags) {
		t.Errorf("GetThread = %+v", got)
	}
}

// TestStore_CreateThread_InvalidInput_RejectedAndNothingStored checks every
// validation rule and that a rejected thread leaves no rows behind.
func TestStore_CreateThread_InvalidInput_RejectedAndNothingStored(t *testing.T) {
	cases := []struct {
		name, title, body string
		tags              []string
		field             string
	}{
		{"no tag", "Title", "Body", nil, "tags"},
		{"blank tag only", "Title", "Body", []string{" "}, "tags"},
		{"unknown tag", "Title", "Body", []string{"ai", "nonsense"}, "tags"},
		{"retired tag", "Title", "Body", []string{"careers"}, "tags"},
		{"empty title", "", "Body", []string{"ai"}, "title"},
		{"blank title", "   ", "Body", []string{"ai"}, "title"},
		{"long title", strings.Repeat("x", MaxTitleLen+1), "Body", []string{"ai"}, "title"},
		{"empty body", "Title", " \n ", []string{"ai"}, "body"},
		{"long body", "Title", strings.Repeat("x", MaxBodyLen+1), []string{"ai"}, "body"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, d := newTestStore(t)
			sam := addMember(t, d, "U0SAM", "Sam R")
			exec(t, d, `UPDATE tags SET active = 0 WHERE slug = 'careers'`)

			_, err := s.CreateThread(context.Background(), sam, tc.title, tc.body, tc.tags)
			var ve ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %v, want a ValidationError", err)
			}
			if ve.Field != tc.field || ve.Message == "" {
				t.Errorf("ValidationError = %+v, want field %q and a message", ve, tc.field)
			}
			if n := countRows(t, d, "threads") + countRows(t, d, "thread_tags"); n != 0 {
				t.Errorf("rows stored after rejection = %d", n)
			}
		})
	}
}

// TestStore_CreateThread_DuplicateTags_StoredOnce checks that repeating a
// tag in the form does not break the insert.
func TestStore_CreateThread_DuplicateTags_StoredOnce(t *testing.T) {
	s, d := newTestStore(t)
	sam := addMember(t, d, "U0SAM", "Sam R")
	id := mustCreate(t, s, sam, "Dupes", "go", "go", " go ")
	got, _ := s.GetThread(context.Background(), id, 0)
	if !slices.Equal(got.Tags, []string{"go"}) {
		t.Errorf("tags = %v, want [go]", got.Tags)
	}
}

// TestStore_ListThreads_ActivityAndScoreOrder checks that fresh activity on
// an older thread lifts it under recent-activity order and that upvote
// order ranks by score. Replies and votes land in a later unit, so this
// test writes the columns they maintain directly.
func TestStore_ListThreads_ActivityAndScoreOrder(t *testing.T) {
	s, d := newTestStore(t)
	sam := addMember(t, d, "U0SAM", "Sam R")
	older := mustCreate(t, s, sam, "Older", "go")
	newer := mustCreate(t, s, sam, "Newer", "go")
	mustCreate(t, s, sam, "Newest", "go")

	threads, _ := mustList(t, s, ListOptions{Sort: SortActivity})
	if got := titles(threads); !slices.Equal(got, []string{"Newest", "Newer", "Older"}) {
		t.Fatalf("activity order before reply = %v", got)
	}

	exec(t, d, `UPDATE threads SET reply_count = 1, last_activity_at = ? WHERE id = ?`, s.Now().Unix(), older)
	threads, _ = mustList(t, s, ListOptions{Sort: SortActivity})
	if got := titles(threads); !slices.Equal(got, []string{"Older", "Newest", "Newer"}) {
		t.Errorf("activity order after reply = %v", got)
	}
	if threads[0].ReplyCount != 1 || !threads[0].LastActivityAt.After(threads[0].CreatedAt) {
		t.Errorf("replied thread = %+v", threads[0])
	}

	exec(t, d, `UPDATE threads SET score = 5 WHERE id = ?`, newer)
	exec(t, d, `UPDATE threads SET score = 2 WHERE id = ?`, older)
	threads, _ = mustList(t, s, ListOptions{Sort: SortScore})
	if got := titles(threads); !slices.Equal(got, []string{"Newer", "Older", "Newest"}) {
		t.Errorf("score order = %v", got)
	}
}

// TestStore_ListThreads_TagFilter_OnlyThreadsCarryingIt checks the filter,
// including a thread with several tags and a tag nobody used.
func TestStore_ListThreads_TagFilter_OnlyThreadsCarryingIt(t *testing.T) {
	s, d := newTestStore(t)
	sam := addMember(t, d, "U0SAM", "Sam R")
	mustCreate(t, s, sam, "Go only", "go")
	mustCreate(t, s, sam, "AI and go", "ai", "go")
	mustCreate(t, s, sam, "Careers", "careers")

	threads, _ := mustList(t, s, ListOptions{Tag: "go"})
	if got := titles(threads); !slices.Equal(got, []string{"AI and go", "Go only"}) {
		t.Errorf("go filter = %v", got)
	}
	if !slices.Equal(threads[0].Tags, []string{"ai", "go"}) {
		t.Errorf("filtered thread lost its other tags: %v", threads[0].Tags)
	}
	for _, tag := range []string{"kubernetes", "no-such-tag"} {
		if threads, _ := mustList(t, s, ListOptions{Tag: tag}); len(threads) != 0 {
			t.Errorf("filter %q = %v, want none", tag, titles(threads))
		}
	}
}

// TestStore_ListThreads_Pagination checks page boundaries, the has-more
// flag, and that a page past the end is empty rather than an error.
func TestStore_ListThreads_Pagination(t *testing.T) {
	s, d := newTestStore(t)
	sam := addMember(t, d, "U0SAM", "Sam R")
	for i := 1; i <= 5; i++ {
		mustCreate(t, s, sam, fmt.Sprintf("Thread %d", i), "general")
	}

	page1, more1 := mustList(t, s, ListOptions{Page: 1, PageSize: 2})
	page3, more3 := mustList(t, s, ListOptions{Page: 3, PageSize: 2})
	page4, more4 := mustList(t, s, ListOptions{Page: 4, PageSize: 2})
	if got := titles(page1); !slices.Equal(got, []string{"Thread 5", "Thread 4"}) || !more1 {
		t.Errorf("page 1 = %v more = %v", got, more1)
	}
	if got := titles(page3); !slices.Equal(got, []string{"Thread 1"}) || more3 {
		t.Errorf("page 3 = %v more = %v", got, more3)
	}
	if len(page4) != 0 || more4 {
		t.Errorf("page 4 = %v more = %v, want empty", titles(page4), more4)
	}
	// Page 0 and negative pages mean the first page.
	if page0, _ := mustList(t, s, ListOptions{Page: -3, PageSize: 2}); !slices.Equal(titles(page0), titles(page1)) {
		t.Errorf("page -3 = %v, want page 1", titles(page0))
	}
	exact, more := mustList(t, s, ListOptions{Page: 1, PageSize: 5})
	if len(exact) != 5 || more {
		t.Errorf("exact page = %d threads more = %v", len(exact), more)
	}
}

// TestStore_ListThreads_DeletedThread_AbsentFromFeed checks soft deletion
// (plan KTD12): the thread leaves every feed view but GetThread still
// returns it, marked deleted, so its page can show a removed notice.
func TestStore_ListThreads_DeletedThread_AbsentFromFeed(t *testing.T) {
	s, d := newTestStore(t)
	sam := addMember(t, d, "U0SAM", "Sam R")
	mustCreate(t, s, sam, "Stays", "go")
	gone := mustCreate(t, s, sam, "Removed", "go")
	exec(t, d, `UPDATE threads SET deleted_at = ? WHERE id = ?`, s.Now().Unix(), gone)

	for _, opts := range []ListOptions{{}, {Sort: SortScore}, {Tag: "go"}} {
		threads, _ := mustList(t, s, opts)
		if got := titles(threads); !slices.Equal(got, []string{"Stays"}) {
			t.Errorf("feed %+v = %v, want only the live thread", opts, got)
		}
	}
	got, err := s.GetThread(context.Background(), gone, sam)
	if err != nil || got.DeletedAt == nil {
		t.Errorf("GetThread(deleted) = %+v, %v; want it returned with DeletedAt set", got, err)
	}
}

// TestStore_GetThread_Missing_ReturnsErrNotFound checks the typed error.
func TestStore_GetThread_Missing_ReturnsErrNotFound(t *testing.T) {
	s, _ := newTestStore(t)
	if _, err := s.GetThread(context.Background(), 999, 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// TestStore_Threads_VotedReflectsViewer checks that Voted is true only for
// the member who holds the vote.
func TestStore_Threads_VotedReflectsViewer(t *testing.T) {
	s, d := newTestStore(t)
	sam := addMember(t, d, "U0SAM", "Sam R")
	priya := addMember(t, d, "U0PRIYA", "Priya N")
	id := mustCreate(t, s, sam, "Voted on", "go")
	exec(t, d, `INSERT INTO votes (member_id, target_kind, target_id, created_at) VALUES (?, 'thread', ?, 1)`, priya, id)
	// A reply vote with the same numeric ID must not count.
	exec(t, d, `INSERT INTO votes (member_id, target_kind, target_id, created_at) VALUES (?, 'reply', ?, 1)`, sam, id)

	for viewer, want := range map[int64]bool{priya: true, sam: false, 0: false} {
		got, err := s.GetThread(context.Background(), id, viewer)
		if err != nil || got.Voted != want {
			t.Errorf("GetThread viewer %d: Voted = %v err = %v, want %v", viewer, got.Voted, err, want)
		}
		threads, _ := mustList(t, s, ListOptions{ViewerID: viewer})
		if threads[0].Voted != want {
			t.Errorf("ListThreads viewer %d: Voted = %v, want %v", viewer, threads[0].Voted, want)
		}
	}
}

// TestStore_GetThread_SlackSource_Exposed checks the fields a moved thread
// carries for its "moved from Slack" notice.
func TestStore_GetThread_SlackSource_Exposed(t *testing.T) {
	s, d := newTestStore(t)
	sam := addMember(t, d, "U0SAM", "Sam R")
	var id int64
	err := d.WithWriteTx(context.Background(), func(tx *sql.Tx) error {
		var err error
		id, err = s.createThreadTx(context.Background(), tx, newThread{
			AuthorID: sam, Title: "From Slack", BodyMD: "Body", TagSlugs: []string{"ai"},
			Slack: &SlackSource{ChannelID: "C1", ChannelName: "ai-driven-development", ThreadTS: "1700000000.000100",
				Permalink: "https://forge.slack.com/archives/C1/p1700000000000100", Remaining: 38, MovedBy: sam},
		})
		return err
	})
	if err != nil {
		t.Fatalf("createThreadTx: %v", err)
	}
	got, err := s.GetThread(context.Background(), id, sam)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if !got.FromSlack || got.SlackChannelName != "ai-driven-development" || got.SlackRemaining != 38 ||
		got.SlackPermalink != "https://forge.slack.com/archives/C1/p1700000000000100" {
		t.Errorf("slack fields = %+v", got)
	}
}

// TestStore_ActiveTags_ExcludesRetired checks the curated list (plan R6).
func TestStore_ActiveTags_ExcludesRetired(t *testing.T) {
	s, d := newTestStore(t)
	exec(t, d, `UPDATE tags SET active = 0 WHERE slug = 'general'`)
	tags, err := s.ActiveTags(context.Background())
	if err != nil {
		t.Fatalf("ActiveTags: %v", err)
	}
	want := []string{"ai", "architecture", "go", "kubernetes", "data-engineering", "forge-cloud", "careers"}
	if !slices.Equal(tags, want) {
		t.Errorf("ActiveTags = %v, want %v", tags, want)
	}
}
