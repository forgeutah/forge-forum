package web

import (
	"errors"
	"html/template"
	"net/http"
	"strconv"

	"github.com/forgeutah/forge-forum/internal/forum"
	"github.com/forgeutah/forge-forum/internal/identity"
	"github.com/forgeutah/forge-forum/internal/markdown"
)

// maxIndentLevels is how many levels of replies indent under their
// parent. Deeper replies stay nested in the markup but stop moving
// right, so a long exchange stays readable on a phone.
const maxIndentLevels = 3

// voteView is the data for the "vote" template: one upvote button that
// posts to Action.
type voteView struct {
	Action string
	// Class is the button's CSS class: "vote", "vote vote--lg", or
	// "vote vote--inline".
	Class string
	Score int
	Voted bool
	// Return, when set, is the local address to go back to after voting.
	Return string
	CSRF   string
}

// newVoteView takes the request's CSRF token, which Page.CSRF holds, so
// a page with many buttons computes it once.
func newVoteView(csrf, action, class string, score int, voted bool) voteView {
	return voteView{Action: action, Class: class, Score: score, Voted: voted, CSRF: csrf}
}

// replyDraft is a reply that was refused, kept so its form can show the
// message and what the member typed.
type replyDraft struct {
	// ParentID is the reply being answered, or 0 for the thread.
	ParentID int64
	Body     string
	Error    string
}

// replyView is the data for the recursive "reply" template.
type replyView struct {
	N        *forum.ReplyNode
	ThreadID int64
	Body     template.HTML
	Vote     voteView
	// ByThreadAuthor adds the "author" badge.
	ByThreadAuthor bool
	// Mine shows edit and delete; CanRemove shows an admin's remove.
	Mine      bool
	CanRemove bool
	// ReplyingTo names the parent's author on replies too deep to show
	// it by indentation.
	ReplyingTo string
	// FlatChildren stops the children indenting further.
	FlatChildren bool
	Children     []replyView
	Draft        replyDraft
	CSRF         string
}

type threadView struct {
	Page
	Thread forum.Thread
	// Body is the thread's Markdown, rendered and sanitized.
	Body template.HTML
	Vote voteView
	// Mine shows edit and delete; CanRemove shows an admin's remove.
	Mine      bool
	CanRemove bool
	Replies   []replyView
	Counts    forum.ReplyCounts
	// Draft is a refused reply to the thread itself.
	Draft replyDraft
	// NotifiesAuthor is true when a reply to the thread can message its
	// author in Slack.
	NotifiesAuthor bool
}

// pathID reads the {id} path value; ok is false for anything but a
// positive integer.
func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

func (s *Server) handleThread(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		s.handleNotFound(w, r)
		return
	}
	s.renderThread(w, r, http.StatusOK, id, replyDraft{})
}

// renderThread shows one thread with its reply tree. draft carries a
// refused reply back to the form it came from.
func (s *Server) renderThread(w http.ResponseWriter, r *http.Request, status int, id int64, draft replyDraft) {
	m := member(r)
	t, err := s.store.GetThread(r.Context(), id, m.ID)
	if errors.Is(err, forum.ErrNotFound) {
		s.handleNotFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if t.DeletedAt != nil {
		s.renderMessage(w, r, http.StatusGone, "Thread removed", "This thread was removed. Its author or a Forge admin took it down.")
		return
	}
	tree, counts, err := s.store.ReplyTree(r.Context(), id, m.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	page := s.page(r, t.Title, "")
	view := threadView{
		Page:           page,
		Thread:         t,
		Body:           markdown.Render(t.BodyMD),
		Vote:           newVoteView(page.CSRF, forum.ThreadPath(id)+"/vote", "vote vote--lg", t.Score, t.Voted),
		Mine:           t.AuthorID == m.ID,
		CanRemove:      m.IsAdmin && t.AuthorID != m.ID,
		Counts:         counts,
		NotifiesAuthor: s.cfg.SlackEnabled() && t.AuthorID != m.ID,
	}
	placed := false
	view.Replies = replyViews(tree, nil, t, m, page.CSRF, draft, &placed)
	if !placed {
		// A refused reply to the thread, or to a reply that has since gone.
		view.Draft = draft
	}
	s.render(w, r, status, "thread", view)
}

// replyViews turns one level of the tree into template data. parent is
// nil at the top level. placed is set once draft has been given to the
// reply it answers. csrf is the request's token.
func replyViews(nodes []*forum.ReplyNode, parent *forum.ReplyNode, t forum.Thread, m identity.Member, csrf string, draft replyDraft, placed *bool) []replyView {
	views := make([]replyView, 0, len(nodes))
	for _, n := range nodes {
		v := replyView{
			N:            n,
			ThreadID:     t.ID,
			FlatChildren: n.Depth+1 >= maxIndentLevels,
			CSRF:         csrf,
		}
		if !n.Deleted {
			v.Body = markdown.Render(n.BodyMD)
			v.Vote = newVoteView(csrf, "/replies/"+strconv.FormatInt(n.ID, 10)+"/vote", "vote vote--inline", n.Score, n.Voted)
			v.ByThreadAuthor = n.AuthorID == t.AuthorID
			v.Mine = n.AuthorID == m.ID
			v.CanRemove = m.IsAdmin && !v.Mine
			if n.Depth >= maxIndentLevels && parent != nil {
				v.ReplyingTo = "a deleted reply"
				if !parent.Deleted {
					v.ReplyingTo = "@" + parent.AuthorName
				}
			}
			if draft.Error != "" && draft.ParentID == n.ID {
				v.Draft, *placed = draft, true
			}
		}
		v.Children = replyViews(n.Children, n, t, m, csrf, draft, placed)
		views = append(views, v)
	}
	return views
}
