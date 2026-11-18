package forum

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/forgeutah/forge-forum/internal/db"
)

// replyAs posts a reply and returns its ID. parent 0 replies to the thread.
func replyAs(t *testing.T, s *Store, author, thread, parent int64, body string) int64 {
	t.Helper()
	id, err := s.CreateReply(context.Background(), author, thread, parent, body)
	if err != nil {
		t.Fatalf("CreateReply(%q): %v", body, err)
	}
	return id
}

// upvoteN gives a post n upvotes, each from a fresh member.
func upvoteN(t *testing.T, s *Store, d *db.DB, post PostRef, n int) {
	t.Helper()
	for i := range n {
		voter := addMember(t, d, fmt.Sprintf("UV%s%d_%d", post.Kind, post.ID, i), "Voter")
		if _, err := s.Vote(context.Background(), voter, post, true); err != nil {
			t.Fatalf("Vote(%v): %v", post, err)
		}
	}
}

func treeOf(t *testing.T, s *Store, thread, viewer int64) ([]*ReplyNode, ReplyCounts) {
	t.Helper()
	roots, counts, err := s.ReplyTree(context.Background(), thread, viewer)
	if err != nil {
		t.Fatalf("ReplyTree: %v", err)
	}
	return roots, counts
}

func threadOf(t *testing.T, s *Store, id, viewer int64) Thread {
	t.Helper()
	th, err := s.GetThread(context.Background(), id, viewer)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	return th
}

// outline renders a tree as "body(score)[children]" so one comparison
// checks order and nesting together.
func outline(nodes []*ReplyNode) string {
	var parts []string
	for _, n := range nodes {
		label := n.BodyMD
		switch {
		case n.Removed:
			label = "<removed>"
		case n.Deleted:
			label = "<deleted>"
		}
		part := fmt.Sprintf("%s(%d)", label, n.Score)
		if len(n.Children) > 0 {
			part += "[" + outline(n.Children) + "]"
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, " ")
}

func wantOutline(t *testing.T, s *Store, thread int64, want string) {
	t.Helper()
	roots, _ := treeOf(t, s, thread, 0)
	if got := outline(roots); got != want {
		t.Errorf("tree = %s\nwant   %s", got, want)
	}
}

func replyRef(id int64) PostRef  { return PostRef{Kind: PostReply, ID: id} }
func threadRef(id int64) PostRef { return PostRef{Kind: PostThread, ID: id} }

// TestStore_ReplyTree_AE2_HigherScoreFirstChildNested covers AE2: A has 3
// upvotes, B has 5, C answers A. B sorts above A and C nests under A.
func TestStore_ReplyTree_AE2_HigherScoreFirstChildNested(t *testing.T) {
	s, d := newTestStore(t)
	sam := addMember(t, d, "U0SAM", "Sam")
	th := mustCreate(t, s, sam, "Thread", "go")
	a := replyAs(t, s, sam, th, 0, "A")
	b := replyAs(t, s, sam, th, 0, "B")
	c := replyAs(t, s, sam, th, a, "C")
	upvoteN(t, s, d, replyRef(a), 3)
	upvoteN(t, s, d, replyRef(b), 5)

	wantOutline(t, s, th, "B(5) A(3)[C(0)]")
	roots, counts := treeOf(t, s, th, 0)
	if counts.Live != 3 || counts.Imported != 0 {
		t.Errorf("counts = %+v, want 3 live, 0 imported", counts)
	}
	child := roots[1].Children[0]
	if child.ID != c || child.ParentID != a || child.Depth != 1 || roots[0].Depth != 0 || roots[0].ParentID != 0 {
		t.Errorf("child = %+v, roots[0] = %+v", child, roots[0])
	}
	if child.AuthorName != "Sam" || child.AuthorSlackID != "U0SAM" || child.AuthorID != sam {
		t.Errorf("author fields = %q %q %d", child.AuthorName, child.AuthorSlackID, child.AuthorID)
	}
}

// TestStore_ReplyTree_EqualScore_OldestFirst checks the R9 tie-break at
// the top level and among the children of one reply.
func TestStore_ReplyTree_EqualScore_OldestFirst(t *testing.T) {
	s, d := newTestStore(t)
	sam := addMember(t, d, "U0SAM", "Sam")
	th := mustCreate(t, s, sam, "Thread", "go")
	first := replyAs(t, s, sam, th, 0, "first")
	second := replyAs(t, s, sam, th, 0, "second")
	replyAs(t, s, sam, th, 0, "third")
	replyAs(t, s, sam, th, first, "child one")
	replyAs(t, s, sam, th, first, "child two")
	upvoteN(t, s, d, replyRef(first), 1)
	upvoteN(t, s, d, replyRef(second), 1)

	wantOutline(t, s, th, "first(1)[child one(0) child two(0)] second(1) third(0)")
}

// TestStore_ReplyTree_ImportedAndVoted_Reported checks the imported mark,
// the imported count, and that Voted follows the viewer.
func TestStore_ReplyTree_ImportedAndVoted_Reported(t *testing.T) {
	s, d := newTestStore(t)
	sam := addMember(t, d, "U0SAM", "Sam")
	th := mustCreate(t, s, sam, "Thread", "go")
	native := replyAs(t, s, sam, th, 0, "native")
	exec(t, d, `INSERT INTO replies (thread_id, author_id, body_md, created_at, imported, slack_ts)
		VALUES (?, ?, 'from slack', 1, 1, '1.5')`, th, sam)
	if _, err := s.Vote(context.Background(), sam, replyRef(native), true); err != nil {
		t.Fatal(err)
	}

	roots, counts := treeOf(t, s, th, sam)
	if counts.Imported != 1 || counts.Live != 2 {
		t.Errorf("counts = %+v, want 2 live, 1 imported", counts)
	}
	if !roots[0].Voted || roots[0].Imported || roots[1].Voted || !roots[1].Imported {
		t.Errorf("roots = %+v, %+v", roots[0], roots[1])
	}
	if other, _ := treeOf(t, s, th, 0); other[0].Voted {
		t.Error("Voted is set for a viewer who did not vote")
	}
}

// TestStore_Vote_TwiceThenWithdraw_OneVoteThenPriorScore covers R8 on a
// thread and a reply, including a member upvoting their own post.
func TestStore_Vote_TwiceThenWithdraw_OneVoteThenPriorScore(t *testing.T) {
	s, d := newTestStore(t)
	ctx := context.Background()
	sam := addMember(t, d, "U0SAM", "Sam")
	th := mustCreate(t, s, sam, "Thread", "go")
	r := replyAs(t, s, sam, th, 0, "reply")
	upvoteN(t, s, d, threadRef(th), 2)
	upvoteN(t, s, d, replyRef(r), 2)

	score := func(post PostRef) (int, bool) {
		if post.Kind == PostThread {
			got := threadOf(t, s, th, sam)
			return got.Score, got.Voted
		}
		roots, _ := treeOf(t, s, th, sam)
		return roots[0].Score, roots[0].Voted
	}
	for _, post := range []PostRef{threadRef(th), replyRef(r)} {
		for range 2 {
			gotThread, err := s.Vote(ctx, sam, post, true)
			if err != nil || gotThread != th {
				t.Fatalf("Vote(%v) = %d, %v", post, gotThread, err)
			}
		}
		if n, voted := score(post); n != 3 || !voted {
			t.Errorf("%v after two upvotes: score %d voted %v, want 3 true", post, n, voted)
		}
		for range 2 {
			if _, err := s.Vote(ctx, sam, post, false); err != nil {
				t.Fatalf("withdraw %v: %v", post, err)
			}
		}
		if n, voted := score(post); n != 2 || voted {
			t.Errorf("%v after withdrawing: score %d voted %v, want 2 false", post, n, voted)
		}
	}
	if n := countRows(t, d, "votes"); n != 4 {
		t.Errorf("vote rows = %d, want 4", n)
	}
}

// TestStore_Vote_DeletedOrMissingTarget_Refused checks votes cannot land
// on posts that are gone, or on anything inside a deleted thread.
func TestStore_Vote_DeletedOrMissingTarget_Refused(t *testing.T) {
	s, d := newTestStore(t)
	ctx := context.Background()
	sam := addMember(t, d, "U0SAM", "Sam")
	th := mustCreate(t, s, sam, "Thread", "go")
	gone := replyAs(t, s, sam, th, 0, "gone")
	if _, err := s.DeletePost(ctx, Actor{ID: sam}, replyRef(gone)); err != nil {
		t.Fatal(err)
	}
	dead := mustCreate(t, s, sam, "Dead thread", "go")
	inDead := replyAs(t, s, sam, dead, 0, "in dead thread")
	if _, err := s.DeletePost(ctx, Actor{ID: sam}, threadRef(dead)); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		post PostRef
		want error
	}{
		{"missing thread", threadRef(999), ErrNotFound},
		{"missing reply", replyRef(999), ErrNotFound},
		{"deleted reply", replyRef(gone), ErrDeleted},
		{"deleted thread", threadRef(dead), ErrDeleted},
		{"reply in deleted thread", replyRef(inDead), ErrDeleted},
	}
	for _, tc := range cases {
		if _, err := s.Vote(ctx, sam, tc.post, true); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
	if _, err := s.Vote(ctx, sam, PostRef{Kind: "comment", ID: th}, true); err == nil {
		t.Error("unknown post kind accepted")
	}
	if n := countRows(t, d, "votes"); n != 0 {
		t.Errorf("vote rows = %d, want 0", n)
	}
}

// TestStore_DeletePost_AE4_ReplyWithTwoReplies_LeavesPlaceholder covers
// AE4: the deleted reply keeps its position, withholds its author and
// body, and both replies stay nested under it.
func TestStore_DeletePost_AE4_ReplyWithTwoReplies_LeavesPlaceholder(t *testing.T) {
	s, d := newTestStore(t)
	sam := addMember(t, d, "U0SAM", "Sam")
	dana := addMember(t, d, "U0DANA", "Dana")
	th := mustCreate(t, s, sam, "Thread", "go")
	parent := replyAs(t, s, dana, th, 0, "secret words")
	replyAs(t, s, sam, th, parent, "one")
	replyAs(t, s, sam, th, parent, "two")
	replyAs(t, s, sam, th, 0, "bystander")
	upvoteN(t, s, d, replyRef(parent), 2)

	gotThread, err := s.DeletePost(context.Background(), Actor{ID: dana}, replyRef(parent))
	if err != nil || gotThread != th {
		t.Fatalf("DeletePost = %d, %v", gotThread, err)
	}
	// Still first: the placeholder stands where the reply stood.
	wantOutline(t, s, th, "<deleted>(0)[one(0) two(0)] bystander(0)")
	roots, counts := treeOf(t, s, th, dana)
	ph := roots[0]
	if !ph.Deleted || ph.Removed || ph.BodyMD != "" || ph.AuthorID != 0 || ph.AuthorName != "" || ph.AuthorSlackID != "" || ph.ID != parent {
		t.Errorf("placeholder leaks or is mislabelled: %+v", ph)
	}
	if counts.Live != 3 || threadOf(t, s, th, 0).ReplyCount != 3 {
		t.Errorf("live = %d, reply_count = %d, want 3 and 3", counts.Live, threadOf(t, s, th, 0).ReplyCount)
	}
}

// TestStore_DeletePost_LeafReply_OmittedAndCountDecremented checks a
// deleted leaf disappears, and that a placeholder disappears too once its
// last live descendant is deleted.
func TestStore_DeletePost_LeafReply_OmittedAndCountDecremented(t *testing.T) {
	s, d := newTestStore(t)
	ctx := context.Background()
	sam := addMember(t, d, "U0SAM", "Sam")
	th := mustCreate(t, s, sam, "Thread", "go")
	replyAs(t, s, sam, th, 0, "keep")
	parent := replyAs(t, s, sam, th, 0, "parent")
	mid := replyAs(t, s, sam, th, parent, "mid")
	leaf := replyAs(t, s, sam, th, mid, "leaf")

	for _, id := range []int64{parent, mid} {
		if _, err := s.DeletePost(ctx, Actor{ID: sam}, replyRef(id)); err != nil {
			t.Fatal(err)
		}
	}
	wantOutline(t, s, th, "keep(0) <deleted>(0)[<deleted>(0)[leaf(0)]]")
	if n := threadOf(t, s, th, 0).ReplyCount; n != 2 {
		t.Errorf("reply_count = %d, want 2", n)
	}

	if _, err := s.DeletePost(ctx, Actor{ID: sam}, replyRef(leaf)); err != nil {
		t.Fatal(err)
	}
	wantOutline(t, s, th, "keep(0)")
	if n := threadOf(t, s, th, 0).ReplyCount; n != 1 {
		t.Errorf("reply_count = %d, want 1", n)
	}
	// Deleting twice changes nothing.
	if _, err := s.DeletePost(ctx, Actor{ID: sam}, replyRef(leaf)); !errors.Is(err, ErrDeleted) {
		t.Errorf("second delete: err = %v, want ErrDeleted", err)
	}
	if n := threadOf(t, s, th, 0).ReplyCount; n != 1 {
		t.Errorf("reply_count after second delete = %d, want 1", n)
	}
}

// TestStore_EditReply_NonAuthorRefused_AuthorMarkedEdited covers R10 for
// replies. Being an admin does not let you edit someone else's words.
func TestStore_EditReply_NonAuthorRefused_AuthorMarkedEdited(t *testing.T) {
	s, d := newTestStore(t)
	ctx := context.Background()
	sam := addMember(t, d, "U0SAM", "Sam")
	dana := addMember(t, d, "U0DANA", "Dana")
	th := mustCreate(t, s, sam, "Thread", "go")
	r := replyAs(t, s, dana, th, 0, "original")

	if _, err := s.EditReply(ctx, sam, r, "hijacked"); !errors.Is(err, ErrForbidden) {
		t.Errorf("non-author edit: err = %v, want ErrForbidden", err)
	}
	roots, _ := treeOf(t, s, th, 0)
	if roots[0].BodyMD != "original" || roots[0].EditedAt != nil {
		t.Errorf("reply changed by a refused edit: %+v", roots[0])
	}

	var invalid ValidationError
	if _, err := s.EditReply(ctx, dana, r, "   "); !errors.As(err, &invalid) || invalid.Field != "body" {
		t.Errorf("empty edit: err = %v, want a body ValidationError", err)
	}
	gotThread, err := s.EditReply(ctx, dana, r, "  improved  ")
	if err != nil || gotThread != th {
		t.Fatalf("EditReply = %d, %v", gotThread, err)
	}
	roots, _ = treeOf(t, s, th, 0)
	if roots[0].BodyMD != "improved" || roots[0].EditedAt == nil {
		t.Errorf("after edit: %+v", roots[0])
	}
	got, err := s.GetReply(ctx, r)
	if err != nil || got.BodyMD != "improved" || got.ThreadID != th || got.AuthorID != dana {
		t.Errorf("GetReply = %+v, %v", got, err)
	}
	if _, err := s.GetReply(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetReply(missing): err = %v", err)
	}
}

// TestStore_EditThread_AuthorOnly_UpdatesTitleBodyTags covers R10 for
// threads: title, body, and tags change and the thread is marked edited.
func TestStore_EditThread_AuthorOnly_UpdatesTitleBodyTags(t *testing.T) {
	s, d := newTestStore(t)
	ctx := context.Background()
	sam := addMember(t, d, "U0SAM", "Sam")
	dana := addMember(t, d, "U0DANA", "Dana")
	th := mustCreate(t, s, sam, "Old title", "go", "careers")
	exec(t, d, `UPDATE tags SET active = 0 WHERE slug = 'careers'`)

	if err := s.EditThread(ctx, dana, th, "Stolen", "Body", []string{"go"}); !errors.Is(err, ErrForbidden) {
		t.Errorf("non-author edit: err = %v, want ErrForbidden", err)
	}
	var invalid ValidationError
	if err := s.EditThread(ctx, sam, th, "", "Body", []string{"go"}); !errors.As(err, &invalid) || invalid.Field != "title" {
		t.Errorf("empty title: err = %v", err)
	}
	if err := s.EditThread(ctx, sam, th, "Title", "Body", []string{"forge-cloud", "nope"}); !errors.As(err, &invalid) || invalid.Field != "tags" {
		t.Errorf("unknown tag: err = %v", err)
	}
	if got := threadOf(t, s, th, 0); got.Title != "Old title" || got.EditedAt != nil || !slices.Equal(got.Tags, []string{"go", "careers"}) {
		t.Errorf("thread changed by refused edits: %+v", got)
	}

	// A retired tag the thread already carries may stay; a new one must be active.
	if err := s.EditThread(ctx, sam, th, " New title ", "New body", []string{"careers", "ai"}); err != nil {
		t.Fatalf("EditThread: %v", err)
	}
	got := threadOf(t, s, th, 0)
	if got.Title != "New title" || got.BodyMD != "New body" || got.EditedAt == nil || !slices.Equal(got.Tags, []string{"ai", "careers"}) {
		t.Errorf("after edit: %+v", got)
	}
	if !got.LastActivityAt.Equal(got.CreatedAt) {
		t.Error("an edit counted as activity")
	}
}

// TestStore_DeletePost_NonAuthorRefused_AdminRemoves covers R21: only the
// author or an admin takes a post down, a removed thread leaves the feed,
// and a removed reply is marked as a moderator's removal.
func TestStore_DeletePost_NonAuthorRefused_AdminRemoves(t *testing.T) {
	s, d := newTestStore(t)
	ctx := context.Background()
	sam := addMember(t, d, "U0SAM", "Sam")
	dana := addMember(t, d, "U0DANA", "Dana")
	admin := addMember(t, d, "U0ADMIN", "Admin")
	th := mustCreate(t, s, sam, "Sam's thread", "go")
	mustCreate(t, s, sam, "Other thread", "go")
	r := replyAs(t, s, sam, th, 0, "rude")
	replyAs(t, s, dana, th, r, "answer")

	for _, post := range []PostRef{threadRef(th), replyRef(r)} {
		if _, err := s.DeletePost(ctx, Actor{ID: dana}, post); !errors.Is(err, ErrForbidden) {
			t.Errorf("non-admin removing %v: err = %v, want ErrForbidden", post, err)
		}
	}
	if list, _ := mustList(t, s, ListOptions{}); len(list) != 2 {
		t.Fatalf("feed has %d threads after refused removals, want 2", len(list))
	}

	if _, err := s.DeletePost(ctx, Actor{ID: admin, IsAdmin: true}, replyRef(r)); err != nil {
		t.Fatalf("admin remove reply: %v", err)
	}
	wantOutline(t, s, th, "<removed>(0)[answer(0)]")

	if _, err := s.DeletePost(ctx, Actor{ID: admin, IsAdmin: true}, threadRef(th)); err != nil {
		t.Fatalf("admin remove thread: %v", err)
	}
	list, _ := mustList(t, s, ListOptions{})
	if got := titles(list); !slices.Equal(got, []string{"Other thread"}) {
		t.Errorf("feed = %v, want only the other thread", got)
	}
	if got := threadOf(t, s, th, 0); got.DeletedAt == nil {
		t.Error("removed thread has no DeletedAt")
	}
	var removedBy int64
	if err := d.Reader().QueryRow(`SELECT removed_by FROM threads WHERE id = ?`, th).Scan(&removedBy); err != nil || removedBy != admin {
		t.Errorf("removed_by = %d, %v, want the admin", removedBy, err)
	}
}

// TestStore_CreateReply_BadTarget_Refused covers the refusals: a parent in
// another thread, a deleted thread, a deleted parent, and missing targets.
func TestStore_CreateReply_BadTarget_Refused(t *testing.T) {
	s, d := newTestStore(t)
	ctx := context.Background()
	sam := addMember(t, d, "U0SAM", "Sam")
	th := mustCreate(t, s, sam, "Thread", "go")
	elsewhere := mustCreate(t, s, sam, "Elsewhere", "go")
	foreign := replyAs(t, s, sam, elsewhere, 0, "in another thread")
	gone := replyAs(t, s, sam, th, 0, "gone")
	if _, err := s.DeletePost(ctx, Actor{ID: sam}, replyRef(gone)); err != nil {
		t.Fatal(err)
	}
	dead := mustCreate(t, s, sam, "Dead", "go")
	if _, err := s.DeletePost(ctx, Actor{ID: sam}, threadRef(dead)); err != nil {
		t.Fatal(err)
	}
	before := countRows(t, d, "replies")

	cases := []struct {
		name           string
		thread, parent int64
		want           error
	}{
		{"parent in another thread", th, foreign, ErrNotFound},
		{"deleted thread", dead, 0, ErrDeleted},
		{"deleted parent", th, gone, ErrDeleted},
		{"missing thread", 999, 0, ErrNotFound},
		{"missing parent", th, 999, ErrNotFound},
	}
	for _, tc := range cases {
		if _, err := s.CreateReply(ctx, sam, tc.thread, tc.parent, "hello"); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
	if n := countRows(t, d, "replies"); n != before {
		t.Errorf("replies = %d, want %d: a refused reply was stored", n, before)
	}
	if n := threadOf(t, s, th, 0).ReplyCount; n != 0 {
		t.Errorf("reply_count = %d, want 0", n)
	}
}

// TestStore_CreateReply_Valid_BumpsCountAndActivity checks a reply updates
// the thread's cached count and activity time, and that bad bodies do not.
func TestStore_CreateReply_Valid_BumpsCountAndActivity(t *testing.T) {
	s, d := newTestStore(t)
	ctx := context.Background()
	sam := addMember(t, d, "U0SAM", "Sam")
	th := mustCreate(t, s, sam, "Thread", "go")
	created := threadOf(t, s, th, 0).LastActivityAt

	var invalid ValidationError
	for _, body := range []string{"", " \n\t ", strings.Repeat("x", MaxBodyLen+1)} {
		if _, err := s.CreateReply(ctx, sam, th, 0, body); !errors.As(err, &invalid) || invalid.Field != "body" {
			t.Errorf("body of %d chars: err = %v, want a body ValidationError", len(body), err)
		}
	}
	if got := threadOf(t, s, th, 0); got.ReplyCount != 0 || !got.LastActivityAt.Equal(created) {
		t.Errorf("refused replies changed the thread: %+v", got)
	}

	top := replyAs(t, s, sam, th, 0, "  top  ")
	replyAs(t, s, sam, th, top, "nested")
	got := threadOf(t, s, th, 0)
	if got.ReplyCount != 2 || !got.LastActivityAt.After(created) {
		t.Errorf("reply_count = %d, activity %v (created %v)", got.ReplyCount, got.LastActivityAt, created)
	}
	wantOutline(t, s, th, "top(0)[nested(0)]")
}

// TestStore_Tags_AddDeactivateReactivate covers R6: admins curate the
// list, retired tags cannot be picked, and threads keep the tags they had.
func TestStore_Tags_AddDeactivateReactivate(t *testing.T) {
	s, d := newTestStore(t)
	ctx := context.Background()
	sam := addMember(t, d, "U0SAM", "Sam")
	admin := Actor{ID: addMember(t, d, "U0ADMIN", "Admin"), IsAdmin: true}
	old := mustCreate(t, s, sam, "Old rust thread", "go")

	if err := s.AddTag(ctx, Actor{ID: sam}, "rust"); !errors.Is(err, ErrForbidden) {
		t.Errorf("non-admin AddTag: err = %v, want ErrForbidden", err)
	}
	if err := s.SetTagActive(ctx, Actor{ID: sam}, "go", false); !errors.Is(err, ErrForbidden) {
		t.Errorf("non-admin SetTagActive: err = %v, want ErrForbidden", err)
	}
	var invalid ValidationError
	for _, slug := range []string{"", "x", "has space", "under_score", "ünï", strings.Repeat("a", 33), "go"} {
		if err := s.AddTag(ctx, admin, slug); !errors.As(err, &invalid) || invalid.Field != "slug" {
			t.Errorf("AddTag(%q): err = %v, want a slug ValidationError", slug, err)
		}
	}
	if err := s.AddTag(ctx, admin, " #Rust-2 "); err != nil {
		t.Fatalf("AddTag: %v", err)
	}
	active, _ := s.ActiveTags(ctx)
	if !slices.Contains(active, "rust-2") {
		t.Errorf("active tags = %v, want rust-2 among them", active)
	}

	if err := s.SetTagActive(ctx, admin, "go", false); err != nil {
		t.Fatalf("SetTagActive: %v", err)
	}
	if err := s.SetTagActive(ctx, admin, "no-such-tag", false); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetTagActive(missing): err = %v, want ErrNotFound", err)
	}
	if active, _ = s.ActiveTags(ctx); slices.Contains(active, "go") {
		t.Errorf("active tags = %v, want go retired", active)
	}
	if _, err := s.CreateThread(ctx, sam, "New", "Body", []string{"go"}); !errors.As(err, &invalid) || invalid.Field != "tags" {
		t.Errorf("thread with a retired tag: err = %v, want a tags ValidationError", err)
	}
	if got := threadOf(t, s, old, 0); !slices.Equal(got.Tags, []string{"go"}) {
		t.Errorf("existing thread's tags = %v, want it to keep go", got.Tags)
	}
	all, err := s.AllTags(ctx)
	if err != nil || len(all) != 9 || all[0] != (Tag{Slug: "general", Active: true}) || all[3] != (Tag{Slug: "go"}) {
		t.Errorf("AllTags = %v, %v", all, err)
	}

	if err := s.SetTagActive(ctx, admin, "go", true); err != nil {
		t.Fatalf("reactivate: %v", err)
	}
	mustCreate(t, s, sam, "Go again", "go")
}

// TestStore_SetDMReplies_Persists covers the R13 setting.
func TestStore_SetDMReplies_Persists(t *testing.T) {
	s, d := newTestStore(t)
	ctx := context.Background()
	sam := addMember(t, d, "U0SAM", "Sam")
	dana := addMember(t, d, "U0DANA", "Dana")

	if on, err := s.DMReplies(ctx, sam); err != nil || !on {
		t.Fatalf("default DMReplies = %v, %v, want on", on, err)
	}
	if err := s.SetDMReplies(ctx, sam, false); err != nil {
		t.Fatal(err)
	}
	if on, _ := s.DMReplies(ctx, sam); on {
		t.Error("DMReplies still on after turning it off")
	}
	if on, _ := s.DMReplies(ctx, dana); !on {
		t.Error("another member's setting changed")
	}
	if err := s.SetDMReplies(ctx, sam, true); err != nil {
		t.Fatal(err)
	}
	if on, _ := s.DMReplies(ctx, sam); !on {
		t.Error("DMReplies off after turning it back on")
	}
	if _, err := s.DMReplies(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("DMReplies(missing): err = %v, want ErrNotFound", err)
	}
}
