package web

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/forgeutah/forge-forum/internal/config"
	"github.com/forgeutah/forge-forum/internal/forum"
	"github.com/forgeutah/forge-forum/internal/notify"
	"github.com/forgeutah/forge-forum/internal/slackx"
)

var (
	dana  = user{slackID: "U0DANA", name: "Dana O"}
	lee   = user{slackID: "U0LEE", name: "Lee W"}
	admin = user{slackID: "U0ADMIN", name: "Ada Admin", roles: "member,admin"}
)

func withSlack(c *config.Config) {
	c.ForgeBotURL, c.ForgeBotToken, c.SlackAnnounceChannel = "http://forge-bot.test:8080", "forum-service-token-0123456789abcdef", "C0ANNOUNCE"
}

// submit posts a form as u with u's CSRF token. kv is field, value pairs.
func (a *app) submit(u user, path string, kv ...string) *httptest.ResponseRecorder {
	a.t.Helper()
	form := url.Values{"csrf_token": {a.token(u)}}
	for i := 0; i+1 < len(kv); i += 2 {
		form.Add(kv[i], kv[i+1])
	}
	return a.post(u, path, form)
}

// mustSubmit is submit for a post that has to succeed with a redirect; it
// returns where the redirect points.
func (a *app) mustSubmit(u user, path string, kv ...string) string {
	a.t.Helper()
	w := a.submit(u, path, kv...)
	if w.Code != http.StatusSeeOther {
		a.t.Fatalf("POST %s %v: status %d, want 303", path, kv, w.Code)
	}
	return w.Header().Get("Location")
}

// reply posts a reply to a thread (parent 0) or to one of its replies and
// returns the new reply's ID.
func (a *app) reply(u user, thread string, parent int64, body string) int64 {
	a.t.Helper()
	kv := []string{"body", body}
	if parent != 0 {
		kv = append(kv, "parent", strconv.FormatInt(parent, 10))
	}
	loc := a.mustSubmit(u, thread+"/replies", kv...)
	prefix := thread + "#reply-"
	id, err := strconv.ParseInt(strings.TrimPrefix(loc, prefix), 10, 64)
	if !strings.HasPrefix(loc, prefix) || err != nil {
		a.t.Fatalf("reply redirected to %q, want %s<id>", loc, prefix)
	}
	return id
}

// upvoteN upvotes a post as n different members.
func (a *app) upvoteN(path string, n int) {
	a.t.Helper()
	for i := range n {
		voter := user{slackID: fmt.Sprintf("U0VOTER%d", i), name: fmt.Sprintf("Voter %d", i)}
		a.mustSubmit(voter, path+"/vote", "vote", "on")
	}
}

func (a *app) count(query string, args ...any) int {
	a.t.Helper()
	var n int
	if err := a.db.Reader().QueryRow(query, args...).Scan(&n); err != nil {
		a.t.Fatalf("count %q: %v", query, err)
	}
	return n
}

func anchor(id int64) string    { return fmt.Sprintf(`id="reply-%d"`, id) }
func replyPath(id int64) string { return fmt.Sprintf("/replies/%d", id) }
func (a *app) page(u user, path string) string {
	a.t.Helper()
	w := a.get(u, path)
	if w.Code != http.StatusOK {
		a.t.Fatalf("GET %s: status %d", path, w.Code)
	}
	return w.Body.String()
}

// TestServer_Thread_AE2_HigherScoreFirstChildNested covers AE2 on the
// rendered page: B (5 upvotes) sits above A (3), and C sits inside A.
func TestServer_Thread_AE2_HigherScoreFirstChildNested(t *testing.T) {
	a := newApp(t)
	th := a.createThread(sam, "Factories", "Body", "ai")
	ra := a.reply(dana, th, 0, "Reply Alpha")
	rb := a.reply(lee, th, 0, "Reply Bravo")
	rc := a.reply(sam, th, ra, "Reply Charlie")
	a.upvoteN(replyPath(ra), 3)
	a.upvoteN(replyPath(rb), 5)

	body := a.page(sam, th)
	wantOrder(t, body,
		`aria-label="Reply from Lee W"`, anchor(rb), "Reply Bravo", `aria-label="Upvote, 5 upvotes"`,
		`aria-label="Reply from Dana O"`, anchor(ra), "Reply Alpha", `aria-label="Upvote, 3 upvotes"`,
		`<div class="reply-children">`, anchor(rc), "Reply Charlie", `aria-label="Upvote, 0 upvotes"`,
		`aria-label="Write a reply"`)
	if n := strings.Count(body, `<section class="reply-card"`); n != 2 {
		t.Errorf("page has %d top-level reply cards, want 2 (C belongs inside A's)", n)
	}
	wantContains(t, body, "3 replies", "sorted by upvotes at each level", "Reply to the thread", "Post reply",
		`<span class="badge">author</span>`)
	if n := strings.Count(body, `<span class="badge">author</span>`); n != 1 {
		t.Errorf("author badge shown %d times, want once (on Sam's reply)", n)
	}
	// A voter sees their own vote on the button.
	voter := user{slackID: "U0VOTER0", name: "Voter 0"}
	wantContains(t, a.page(voter, th), `aria-pressed="true" aria-label="Remove your upvote, 5 upvotes"`)
}

// TestServer_Vote_TwiceThenWithdraw_OneVoteThenPriorScore covers R8
// through the forms: a repeated upvote counts once, withdrawing undoes it,
// and the redirect lands on the post that was voted on.
func TestServer_Vote_TwiceThenWithdraw_OneVoteThenPriorScore(t *testing.T) {
	a := newApp(t)
	th := a.createThread(sam, "Factories", "Body", "ai")
	r := a.reply(dana, th, 0, "A reply")
	rp := replyPath(r)

	for range 2 {
		if loc := a.mustSubmit(lee, th+"/vote", "vote", "on"); loc != th {
			t.Errorf("thread vote redirected to %q, want %q", loc, th)
		}
		if loc := a.mustSubmit(lee, rp+"/vote", "vote", "on"); loc != th+"#reply-"+strconv.FormatInt(r, 10) {
			t.Errorf("reply vote redirected to %q", loc)
		}
	}
	body := a.page(lee, th)
	if n := strings.Count(body, `aria-pressed="true" aria-label="Remove your upvote, 1 upvote"`); n != 2 {
		t.Errorf("%d buttons show Lee's single upvote, want 2 (thread and reply)", n)
	}
	wantContains(t, body, `name="vote" value="off"`)
	// Members may upvote their own posts.
	a.mustSubmit(sam, th+"/vote", "vote", "on")
	wantContains(t, a.page(sam, th), `aria-label="Remove your upvote, 2 upvotes"`)

	a.mustSubmit(lee, th+"/vote", "vote", "off")
	a.mustSubmit(lee, th+"/vote", "vote", "off")
	a.mustSubmit(lee, rp+"/vote", "vote", "off")
	body = a.page(lee, th)
	wantContains(t, body, `aria-pressed="false" aria-label="Upvote, 1 upvote"`, `aria-pressed="false" aria-label="Upvote, 0 upvotes"`)
	if n := a.count(`SELECT COUNT(*) FROM votes`); n != 1 {
		t.Errorf("vote rows = %d, want 1 (Sam's)", n)
	}

	for _, path := range []string{"/threads/999/vote", "/replies/999/vote", "/replies/abc/vote"} {
		if w := a.submit(lee, path, "vote", "on"); w.Code != http.StatusNotFound {
			t.Errorf("POST %s: status %d, want 404", path, w.Code)
		}
	}
}

// TestServer_FeedVote_ReturnsToFeed_OpenRedirectIgnored checks the feed's
// upvote button comes back to the same feed page and that a forged
// "return" value cannot send the member off the site.
func TestServer_FeedVote_ReturnsToFeed_OpenRedirectIgnored(t *testing.T) {
	a := newApp(t)
	th := a.createThread(sam, "Factories", "Body", "ai")

	feed := a.page(lee, "/?tag=ai&sort=top")
	wantContains(t, feed, `<form class="vote-form" method="post" action="`+th+`/vote">`,
		`name="return" value="/?sort=top&amp;tag=ai#thread-1"`, `aria-pressed="false" aria-label="Upvote, 0 upvotes"`, `id="thread-1"`)

	back := "/?sort=top&tag=ai#thread-1"
	if loc := a.mustSubmit(lee, th+"/vote", "vote", "on", "return", back); loc != back {
		t.Errorf("feed vote redirected to %q, want %q", loc, back)
	}
	wantContains(t, a.page(lee, "/"), `aria-pressed="true" aria-label="Remove your upvote, 1 upvote"`)

	for _, evil := range []string{
		"https://evil.example.com/", "//evil.example.com/", `/\evil.example.com`, "evil", "javascript:alert(1)", "/ok\r\nX-Evil: 1",
	} {
		if loc := a.mustSubmit(lee, th+"/vote", "vote", "on", "return", evil); loc != th {
			t.Errorf("return=%q redirected to %q, want the thread %q", evil, loc, th)
		}
	}
}

// TestServer_DeleteReply_AE4_PlaceholderKeepsChildren covers AE4 on the
// page: the deleted reply becomes a placeholder with both replies still
// nested under it, and deleting a leaf removes it outright.
func TestServer_DeleteReply_AE4_PlaceholderKeepsChildren(t *testing.T) {
	a := newApp(t)
	th := a.createThread(sam, "Factories", "Body", "ai")
	parent := a.reply(dana, th, 0, "Regrettable words")
	one := a.reply(sam, th, parent, "Child one")
	two := a.reply(lee, th, parent, "Child two")
	leaf := a.reply(lee, th, 0, "Lonely leaf")

	// The author is offered a delete that needs a second, deliberate press.
	wantContains(t, a.page(dana, th), `action="`+replyPath(parent)+`/delete"`, "Delete this reply? Replies under it stay.", "Delete reply")

	if loc := a.mustSubmit(dana, replyPath(parent)+"/delete"); loc != th+"#replies" {
		t.Errorf("delete redirected to %q", loc)
	}
	body := a.page(sam, th)
	wantOrder(t, body, `<section class="reply-card reply-card--deleted" aria-label="Deleted reply">`, anchor(parent),
		"[ reply deleted by its author ]", `<div class="reply-children">`, anchor(one), "Child one", anchor(two), "Child two")
	wantNotContains(t, body, "Regrettable words", "Dana O", replyPath(parent)+"/vote", replyPath(parent)+"/delete")
	wantContains(t, body, "3 replies")

	a.mustSubmit(lee, replyPath(leaf)+"/delete")
	body = a.page(sam, th)
	wantNotContains(t, body, "Lonely leaf", anchor(leaf))
	wantContains(t, body, "2 replies")
	wantContains(t, a.page(sam, "/"), "2 replies")

	// Nothing can be done to a deleted reply.
	for _, path := range []string{"/vote", "/delete", "/edit"} {
		if w := a.submit(dana, replyPath(parent)+path, "body", "x"); w.Code != http.StatusGone {
			t.Errorf("POST %s on a deleted reply: status %d, want 410", path, w.Code)
		}
	}
	if w := a.submit(sam, th+"/replies", "body", "to the void", "parent", strconv.FormatInt(parent, 10)); w.Code != http.StatusGone {
		t.Errorf("reply to a deleted reply: status %d, want 410", w.Code)
	}
}

// TestServer_Edit_AuthorOnly_MarksEdited covers R10: another member is
// refused at the form and at the post, the author's edit shows as edited.
func TestServer_Edit_AuthorOnly_MarksEdited(t *testing.T) {
	a := newApp(t)
	th := a.createThread(sam, "Original title", "Original body", "ai", "go")
	r := a.reply(dana, th, 0, "Original reply")
	rp := replyPath(r)

	// Controls are shown to the author only.
	wantNotContains(t, a.page(lee, th), th+"/edit", rp+"/edit", "/delete")
	wantContains(t, a.page(sam, th), `href="`+th+`/edit"`, `action="`+th+`/delete"`)
	wantNotContains(t, a.page(sam, th), rp+"/edit", rp+"/delete")
	wantContains(t, a.page(dana, th), `href="`+rp+`/edit"`)

	// Hiding the controls is not the check: the store refuses.
	for _, path := range []string{th + "/edit", rp + "/edit"} {
		if w := a.get(lee, path); w.Code != http.StatusForbidden {
			t.Errorf("GET %s as another member: status %d, want 403", path, w.Code)
		}
		if w := a.submit(lee, path, "title", "Hijacked", "body", "Hijacked", "tags", "go"); w.Code != http.StatusForbidden {
			t.Errorf("POST %s as another member: status %d, want 403", path, w.Code)
		}
	}
	for _, path := range []string{th + "/delete", rp + "/delete"} {
		if w := a.submit(lee, path); w.Code != http.StatusForbidden {
			t.Errorf("POST %s as another member: status %d, want 403", path, w.Code)
		}
	}
	body := a.page(lee, th)
	wantContains(t, body, "Original title", "Original body", "Original reply")
	wantNotContains(t, body, "Hijacked", "edited")

	// The reply's author edits it.
	form := a.page(dana, rp+"/edit")
	wantContains(t, form, "Edit your reply", ">Original reply</textarea>", "Save changes", `action="`+rp+`/edit"`)
	wantNotContains(t, form, `name="title"`, `name="tags"`)
	w := a.submit(dana, rp+"/edit", "body", " ")
	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("empty reply edit: status %d, want 422", w.Code)
	}
	wantContains(t, w.Body.String(), "Write something before you post.")
	if loc := a.mustSubmit(dana, rp+"/edit", "body", "Improved reply"); loc != th+"#reply-"+strconv.FormatInt(r, 10) {
		t.Errorf("reply edit redirected to %q", loc)
	}
	body = a.page(lee, th)
	wantContains(t, body, "Improved reply", "· edited")
	wantNotContains(t, body, "Original reply")

	// The thread's author edits title, body, and tags.
	form = a.page(sam, th+"/edit")
	wantContains(t, form, "Edit your thread", `value="Original title"`, ">Original body</textarea>",
		`name="tags" value="ai" checked`, `name="tags" value="go" checked`, `name="tags" value="kubernetes">`)
	w = a.submit(sam, th+"/edit", "title", "", "body", "Kept body", "tags", "kubernetes")
	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("empty title edit: status %d, want 422", w.Code)
	}
	wantContains(t, w.Body.String(), "Give the thread a title.", ">Kept body</textarea>", `name="tags" value="kubernetes" checked`)
	if loc := a.mustSubmit(sam, th+"/edit", "title", "Better title", "body", "Better body", "tags", "kubernetes"); loc != th {
		t.Errorf("thread edit redirected to %q", loc)
	}
	body = a.page(lee, th)
	wantContains(t, body, "Better title", "Better body", "#kubernetes", "edited <time")
	wantNotContains(t, body, "Original title", "#go<")
}

// TestServer_Moderation_AdminRemoves_NonAdminCannot covers R21: an admin
// removes another member's reply and thread; the placeholder says a
// moderator did it and the thread leaves the feed.
func TestServer_Moderation_AdminRemoves_NonAdminCannot(t *testing.T) {
	a := newApp(t)
	th := a.createThread(sam, "Heated thread", "Body", "ai")
	rude := a.reply(dana, th, 0, "Rude words")
	a.reply(lee, th, rude, "Calm answer")

	wantNotContains(t, a.page(lee, th), ">remove<", "Remove thread", "Remove reply")
	body := a.page(admin, th)
	wantContains(t, body, ">remove<", "Remove this thread as a moderator?", "Remove thread", "Remove this reply as a moderator?", "Remove reply")
	wantNotContains(t, body, th+"/edit", replyPath(rude)+"/edit")

	a.mustSubmit(admin, replyPath(rude)+"/delete")
	body = a.page(sam, th)
	wantOrder(t, body, anchor(rude), "[ reply removed by a moderator ]", "Calm answer")
	wantNotContains(t, body, "Rude words", "deleted by its author")

	if loc := a.mustSubmit(admin, th+"/delete"); loc != "/" {
		t.Errorf("thread removal redirected to %q, want the feed", loc)
	}
	wantNotContains(t, a.page(sam, "/"), "Heated thread")
	if w := a.get(sam, th); w.Code != http.StatusGone {
		t.Errorf("removed thread: status %d, want 410", w.Code)
	}
	if n := a.count(`SELECT COUNT(*) FROM threads WHERE removed_by IS NOT NULL`); n != 1 {
		t.Errorf("threads recorded as removed by a moderator = %d, want 1", n)
	}
}

// TestServer_AdminTags_AdminsOnly_CurateTheList covers R6 and the admin
// boundary: the page and its posts are 403 for members; an admin adds,
// retires, and restores tags, and a retired tag is neither offered nor
// accepted while threads that have it keep it.
func TestServer_AdminTags_AdminsOnly_CurateTheList(t *testing.T) {
	a := newApp(t)
	th := a.createThread(sam, "Go thread", "Body", "go")

	if w := a.get(sam, "/admin/tags"); w.Code != http.StatusForbidden {
		t.Errorf("GET /admin/tags as a member: status %d, want 403", w.Code)
	}
	if w := a.submit(sam, "/admin/tags", "slug", "rust"); w.Code != http.StatusForbidden {
		t.Errorf("POST /admin/tags as a member: status %d, want 403", w.Code)
	}
	if w := a.submit(sam, "/admin/tags/go/toggle", "active", "0"); w.Code != http.StatusForbidden {
		t.Errorf("POST toggle as a member: status %d, want 403", w.Code)
	}
	wantContains(t, a.page(sam, "/"), `href="/?tag=go"`)
	wantNotContains(t, a.page(sam, "/"), `href="/admin/tags"`, "#rust")

	wantContains(t, a.page(admin, "/"), `href="/admin/tags"`, "manage tags")
	body := a.page(admin, "/admin/tags")
	wantContains(t, body, "Manage tags", "#kubernetes", `action="/admin/tags/go/toggle"`, "Retire", "Add tag")

	for _, bad := range []string{"x", "Has Space", "under_score", "go"} {
		w := a.submit(admin, "/admin/tags", "slug", bad)
		if w.Code != http.StatusUnprocessableEntity {
			t.Errorf("slug %q: status %d, want 422", bad, w.Code)
		}
		wantContains(t, w.Body.String(), `role="alert"`)
	}
	if loc := a.mustSubmit(admin, "/admin/tags", "slug", "rust"); loc != "/admin/tags" {
		t.Errorf("add redirected to %q", loc)
	}
	wantContains(t, a.page(sam, "/"), `href="/?tag=rust"`)

	a.mustSubmit(admin, "/admin/tags/go/toggle", "active", "0")
	wantContains(t, a.page(admin, "/admin/tags"), "retired", "Restore")
	wantNotContains(t, a.page(sam, "/"), `href="/?tag=go"`)
	wantNotContains(t, a.page(sam, "/threads/new"), `value="go"`)
	if w := a.submit(sam, "/threads", "title", "New go thread", "body", "Body", "tags", "go"); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("thread with a retired tag: status %d, want 422", w.Code)
	}
	wantContains(t, a.page(sam, th), "#go")
	// Its author can still edit the thread and keep the retired tag.
	wantContains(t, a.page(sam, th+"/edit"), `name="tags" value="go" checked`)

	a.mustSubmit(admin, "/admin/tags/go/toggle", "active", "1")
	wantContains(t, a.page(sam, "/"), `href="/?tag=go"`)
	if w := a.submit(admin, "/admin/tags/no-such-tag/toggle", "active", "0"); w.Code != http.StatusNotFound {
		t.Errorf("toggle of a missing tag: status %d, want 404", w.Code)
	}
}

// TestServer_Reply_BadTargetOrBody_Refused covers the reply refusals: a
// parent in another thread, a deleted thread, and an empty body, which
// comes back with the message on the form it was typed into.
func TestServer_Reply_BadTargetOrBody_Refused(t *testing.T) {
	a := newApp(t)
	th := a.createThread(sam, "Factories", "Body", "ai")
	other := a.createThread(sam, "Elsewhere", "Body", "ai")
	foreign := a.reply(dana, other, 0, "In another thread")
	local := a.reply(dana, th, 0, "In this thread")
	before := a.count(`SELECT COUNT(*) FROM replies`)

	if w := a.submit(lee, th+"/replies", "body", "Misfiled", "parent", strconv.FormatInt(foreign, 10)); w.Code != http.StatusNotFound {
		t.Errorf("parent in another thread: status %d, want 404", w.Code)
	}
	if w := a.submit(lee, th+"/replies", "body", "Hello", "parent", "abc"); w.Code != http.StatusNotFound {
		t.Errorf("malformed parent: status %d, want 404", w.Code)
	}
	if w := a.submit(lee, "/threads/999/replies", "body", "Hello"); w.Code != http.StatusNotFound {
		t.Errorf("missing thread: status %d, want 404", w.Code)
	}

	w := a.submit(lee, th+"/replies", "body", "   ")
	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("empty reply: status %d, want 422", w.Code)
	}
	wantOrder(t, w.Body.String(), `aria-label="Write a reply"`, `role="alert"`, "Write something before you post.")

	long := strings.Repeat("x", forum.MaxBodyLen+1)
	w = a.submit(lee, th+"/replies", "body", long, "parent", strconv.FormatInt(local, 10))
	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("oversized nested reply: status %d, want 422", w.Code)
	}
	// The message and the draft come back inside that reply's own form, opened.
	wantOrder(t, w.Body.String(), anchor(local), `<details class="disclosure" open>`, `role="alert"`, "Keep the reply to", "xxxx", `aria-label="Write a reply"`)

	a.mustSubmit(sam, other+"/delete")
	if w := a.submit(lee, other+"/replies", "body", "Too late"); w.Code != http.StatusGone {
		t.Errorf("reply to a deleted thread: status %d, want 410", w.Code)
	}
	if n := a.count(`SELECT COUNT(*) FROM replies`); n != before {
		t.Errorf("replies = %d, want %d: a refused reply was stored", n, before)
	}
}

// TestServer_Thread_MovedFromSlack_ShowsCountsAndBadges checks the moved
// notice's counts, the imported badge, and the byline for a moved thread.
func TestServer_Thread_MovedFromSlack_ShowsCountsAndBadges(t *testing.T) {
	a := newApp(t)
	th := a.createThread(sam, "Moved thread", "Body", "ai")
	a.reply(dana, th, 0, "Native reply")
	a.exec(`UPDATE threads SET slack_channel_id = 'C1', slack_channel_name = 'ai-driven-development',
		slack_thread_ts = '1.2', slack_permalink = 'https://forge.slack.com/archives/C1/p12', slack_remaining = 38`)
	for i := range 12 {
		a.exec(`INSERT INTO replies (thread_id, author_id, body_md, created_at, imported, slack_ts)
			SELECT 1, id, ?, ?, 1, ? FROM members WHERE slack_user_id = 'U0DANA'`,
			fmt.Sprintf("Imported %d", i), 1000+i, fmt.Sprintf("1.%d", i+3))
	}
	a.exec(`UPDATE threads SET reply_count = 13`)

	body := a.page(sam, th)
	wantContains(t, body, "Moved from Slack.", "12 direct replies were imported from #ai-driven-development.",
		"38 more replies stayed in the Slack thread.", "posted in Slack", "13 replies")
	if n := strings.Count(body, "imported from slack"); n != 12 {
		t.Errorf("imported badge shown %d times, want 12", n)
	}
	// Imported replies keep their original order: equal scores, oldest first.
	wantOrder(t, body, "Imported 0", "Imported 1", "Imported 11", "Native reply")
}

// TestServer_Thread_DeepNesting_StopsIndenting checks replies past the
// indent cap stay nested but say who they answer instead of moving right.
func TestServer_Thread_DeepNesting_StopsIndenting(t *testing.T) {
	a := newApp(t)
	th := a.createThread(sam, "Long exchange", "Body", "ai")
	var parent int64
	for i := range 6 {
		u := dana
		if i%2 == 1 {
			u = lee
		}
		parent = a.reply(u, th, parent, fmt.Sprintf("Level %d", i))
	}
	body := a.page(sam, th)
	if got := strings.Count(body, `<div class="reply-children">`); got != maxIndentLevels-1 {
		t.Errorf("%d indented levels, want %d", got, maxIndentLevels-1)
	}
	if got := strings.Count(body, `<div class="reply-children reply-children--flat">`); got != 6-maxIndentLevels {
		t.Errorf("%d flat levels, want %d", got, 6-maxIndentLevels)
	}
	wantOrder(t, body, "Level 2", "replying to @Dana O", "Level 3", "replying to @Lee W", "Level 4", "replying to @Dana O", "Level 5")
	if n := strings.Count(body, "replying to"); n != 3 {
		t.Errorf("%d replies name who they answer, want 3 (levels 3 to 5)", n)
	}
}

// TestServer_Settings_Toggle_Persists covers R13: the checkbox reflects
// and changes dm_replies, and without Slack the page says so instead.
func TestServer_Settings_Toggle_Persists(t *testing.T) {
	a := newApp(t, withSlack)
	checked := `name="dm_replies" value="1" checked`
	body := a.page(dana, "/settings")
	wantContains(t, body, "Send me a Slack message when someone replies to me", checked, "Save settings")

	if loc := a.mustSubmit(dana, "/settings"); loc != "/settings?saved=1" {
		t.Errorf("save redirected to %q", loc)
	}
	body = a.page(dana, "/settings?saved=1")
	wantContains(t, body, "Saved.", `name="dm_replies" value="1">`)
	wantNotContains(t, body, checked)
	wantContains(t, a.page(sam, "/settings"), checked)
	if n := a.count(`SELECT dm_replies FROM members WHERE slack_user_id = 'U0DANA'`); n != 0 {
		t.Errorf("dm_replies = %d, want 0", n)
	}

	a.mustSubmit(dana, "/settings", "dm_replies", "1")
	wantContains(t, a.page(dana, "/settings"), checked)

	off := newApp(t)
	body = off.page(dana, "/settings")
	wantContains(t, body, "Slack is not connected.")
	wantNotContains(t, body, "dm_replies", "Save settings")
	// The composer promises a Slack message only when one can be sent.
	th := off.createThread(sam, "Thread", "Body", "ai")
	wantNotContains(t, off.page(dana, th), "gets a Slack message")
	th = a.createThread(sam, "Thread", "Body", "ai")
	wantContains(t, a.page(dana, th), "@Sam R gets a Slack message when you reply")
	wantNotContains(t, a.page(sam, th), "gets a Slack message")
}

// TestServer_NewPosts_BadCSRF_RejectedAndNothingChanges checks every
// state-changing route added with replies, votes, settings, and admin.
func TestServer_NewPosts_BadCSRF_RejectedAndNothingChanges(t *testing.T) {
	a := newApp(t)
	th := a.createThread(admin, "Admin's thread", "Body", "ai")
	r := a.reply(admin, th, 0, "Admin's reply")
	rp := replyPath(r)

	state := func() string {
		var s string
		err := a.db.Reader().QueryRow(`SELECT
			(SELECT COUNT(*) FROM votes) || ' votes, ' || (SELECT COUNT(*) FROM replies) || ' replies, ' ||
			(SELECT COUNT(*) FROM tags WHERE active = 1) || ' active tags, ' || (SELECT COUNT(*) FROM tags) || ' tags, ' ||
			(SELECT COUNT(*) FROM threads WHERE deleted_at IS NULL AND edited_at IS NULL) || ' untouched threads, ' ||
			(SELECT COUNT(*) FROM replies WHERE deleted_at IS NULL AND edited_at IS NULL) || ' untouched replies, ' ||
			(SELECT SUM(dm_replies) FROM members) || ' dm'`).Scan(&s)
		if err != nil {
			t.Fatalf("state: %v", err)
		}
		return s
	}
	other := a.token(sam)
	before := state()

	routes := []string{
		th + "/vote", th + "/replies", th + "/edit", th + "/delete",
		rp + "/vote", rp + "/edit", rp + "/delete",
		"/settings", "/admin/tags", "/admin/tags/go/toggle",
	}
	form := url.Values{"vote": {"on"}, "body": {"Changed"}, "title": {"Changed"}, "tags": {"go"}, "slug": {"rust"}, "active": {"0"}}
	for _, path := range routes {
		for name, token := range map[string]string{"missing": "", "another member": other} {
			f := url.Values{}
			for k, v := range form {
				f[k] = v
			}
			if token != "" {
				f.Set("csrf_token", token)
			}
			// The admin is the author of everything here, so only the
			// token stands between this request and a change.
			if w := a.post(admin, path, f); w.Code != http.StatusForbidden {
				t.Errorf("POST %s with %s token: status %d, want 403", path, name, w.Code)
			}
		}
	}
	if after := state(); after != before {
		t.Errorf("state changed by rejected posts:\n got %s\nwant %s", after, before)
	}
}

// TestServer_NewPages_FollowCSPAndVoiceRules extends the page-wide checks
// to the pages added with replies: no inline script or style, and copy
// with no exclamation marks or em-dashes.
func TestServer_NewPages_FollowCSPAndVoiceRules(t *testing.T) {
	a := newApp(t, withSlack)
	th := a.createThread(admin, "Any thread", "Body", "go")
	parent := a.reply(admin, th, 0, "Parent")
	a.reply(dana, th, parent, "Child")
	gone := a.reply(dana, th, 0, "Gone")
	a.reply(admin, th, gone, "Under gone")
	a.mustSubmit(admin, replyPath(gone)+"/delete")
	a.mustSubmit(admin, "/admin/tags/careers/toggle", "active", "0")

	inline := regexp.MustCompile(`(?i)<script|<style|\sstyle=|\son[a-z]+=`)
	tags := regexp.MustCompile(`<[^>]*>`)
	for _, path := range []string{th, th + "/edit", replyPath(parent) + "/edit", "/settings", "/settings?saved=1", "/admin/tags"} {
		body := a.page(admin, path)
		if m := inline.FindString(body); m != "" {
			t.Errorf("%s: page contains inline %q", path, m)
		}
		text := tags.ReplaceAllString(body, " ")
		for _, bad := range []string{"!", "—"} {
			if strings.Contains(text, bad) {
				t.Errorf("%s: copy contains %q", path, bad)
			}
		}
	}
	for _, w := range []*httptest.ResponseRecorder{
		a.get(dana, "/admin/tags"), a.get(dana, th+"/edit"), a.get(dana, replyPath(gone)+"/edit"),
		a.submit(dana, th+"/replies", "body", ""),
	} {
		text := tags.ReplaceAllString(w.Body.String(), " ")
		if strings.ContainsAny(text, "!—") || inline.MatchString(w.Body.String()) {
			t.Errorf("status %d page breaks the copy or CSP rules", w.Code)
		}
	}
}

// TestServer_Posts_RealWorker_SlackHearsThreadAndReplies runs the whole
// chain for R11 to R13 and AE3: forms in, the real outbox worker, and a
// fake Slack out.
func TestServer_Posts_RealWorker_SlackHearsThreadAndReplies(t *testing.T) {
	a := newApp(t, withSlack)
	fake := &slackx.Fake{}
	worker := notify.NewWorker(a.db, fake, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Now)
	a.store.SetNotifier(forum.Notifier{BaseURL: "https://forum.example.com", AnnounceChannel: "C0ANNOUNCE", Wake: worker.Wake})
	deliver := func() []slackx.PostedMessage {
		t.Helper()
		if _, err := worker.RunOnce(context.Background()); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
		return fake.PostedMessages()
	}

	th := a.createThread(sam, "Who is building software factories?", "Body", "ai", "architecture")
	posted := deliver()
	if len(posted) != 1 {
		t.Fatalf("Slack received %d messages after a new thread, want 1: %+v", len(posted), posted)
	}
	want := slackx.PostedMessage{Channel: "C0ANNOUNCE", Text: notify.AnnounceText("U0SAM",
		"Who is building software factories?", "https://forum.example.com"+th, []string{"ai", "architecture"})}
	if posted[0] != want {
		t.Errorf("announcement = %+v\nwant %+v", posted[0], want)
	}

	// AE3: Dana answers Sam's thread, then Lee answers Dana's reply.
	danas := a.reply(dana, th, 0, "We run a small one.")
	lees := a.reply(lee, th, danas, "How do you scope tickets?")
	posted = deliver()
	if len(posted) != 3 {
		t.Fatalf("Slack received %d messages, want 3: %+v", len(posted), posted)
	}
	if posted[1].Channel != "U0SAM" || !strings.Contains(posted[1].Text, "Dana O replied to you") {
		t.Errorf("message for Dana's reply = %+v, want a direct message to Sam", posted[1])
	}
	wantDM := slackx.PostedMessage{Channel: "U0DANA", Text: notify.ReplyDMText("Lee W", "Who is building software factories?",
		"How do you scope tickets?", fmt.Sprintf("https://forum.example.com%s#reply-%d", th, lees))}
	if posted[2] != wantDM {
		t.Errorf("message for Lee's reply = %+v\nwant %+v", posted[2], wantDM)
	}

	// Dana turns the messages off; replying to your own post never messages.
	a.mustSubmit(dana, "/settings")
	a.reply(lee, th, danas, "Asking again.")
	a.reply(sam, th, 0, "Adding context to my own thread.")
	if posted = deliver(); len(posted) != 3 {
		t.Errorf("Slack received %d messages, want still 3: %+v", len(posted), posted[3:])
	}
	if n := a.count(`SELECT COUNT(*) FROM notifications WHERE status <> 'sent'`); n != 0 {
		t.Errorf("%d outbox rows not in a sent state", n)
	}
}
