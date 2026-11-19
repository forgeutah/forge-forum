package web

import (
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/forgeutah/forge-forum/internal/forum"
)

// actor is the signed-in member as the store's permission checks see them.
func actor(r *http.Request) forum.Actor {
	m := member(r)
	return forum.Actor{ID: m.ID, IsAdmin: m.IsAdmin}
}

// refuse answers a store error that is the member's to see: a missing or
// removed post, or something they may not do. Anything else is a server
// error.
func (s *Server) refuse(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, forum.ErrNotFound):
		s.handleNotFound(w, r)
	case errors.Is(err, forum.ErrDeleted):
		s.renderMessage(w, r, http.StatusGone, "Post removed", "This post was removed, so it can no longer be changed or answered.")
	case errors.Is(err, forum.ErrForbidden):
		s.renderMessage(w, r, http.StatusForbidden, "Not allowed", "You do not have permission to do that. Authors change their own posts, and Forge admins can remove any post.")
	default:
		s.serverError(w, r, err)
	}
}

// postRef reads the {id} path value as a post of the given kind.
func postRef(r *http.Request, kind forum.PostKind) (forum.PostRef, bool) {
	id, ok := pathID(r)
	return forum.PostRef{Kind: kind, ID: id}, ok
}

// postURL is where a post lives: the thread page, at the reply's anchor
// for a reply.
func postURL(threadID int64, post forum.PostRef) string {
	if post.Kind == forum.PostReply {
		return forum.ReplyPath(threadID, post.ID)
	}
	return forum.ThreadPath(threadID)
}

// localReturn accepts a "return" form value only when it is a path on
// this site, so a forged form cannot turn a redirect into a way off it.
func localReturn(raw string) (string, bool) {
	if !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") || strings.ContainsAny(raw, "\\\r\n\t") {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "" || u.Host != "" {
		return "", false
	}
	return raw, true
}

// handleVote records or withdraws an upvote on a thread or reply. The
// form says which ("vote" is "on" or "off"), so a repeated submit is
// harmless.
func (s *Server) handleVote(kind forum.PostKind) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		post, ok := postRef(r, kind)
		if !ok {
			s.handleNotFound(w, r)
			return
		}
		threadID, err := s.store.Vote(r.Context(), member(r).ID, post, r.PostFormValue("vote") != "off")
		if err != nil {
			s.refuse(w, r, err)
			return
		}
		to := postURL(threadID, post)
		if back, ok := localReturn(r.PostFormValue("return")); ok {
			to = back
		}
		http.Redirect(w, r, to, http.StatusSeeOther)
	}
}

// handleDelete takes down a thread or reply: the author's delete or an
// admin's removal. The store decides which, and whether it is allowed.
func (s *Server) handleDelete(kind forum.PostKind) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		post, ok := postRef(r, kind)
		if !ok {
			s.handleNotFound(w, r)
			return
		}
		threadID, err := s.store.DeletePost(r.Context(), actor(r), post)
		if err != nil {
			s.refuse(w, r, err)
			return
		}
		s.logger.Info("post deleted", "member_id", member(r).ID, "kind", string(kind), "post_id", post.ID)
		to := "/"
		if kind == forum.PostReply {
			to = forum.ThreadPath(threadID) + "#replies"
		}
		http.Redirect(w, r, to, http.StatusSeeOther)
	}
}

func (s *Server) handleReplyCreate(w http.ResponseWriter, r *http.Request) {
	threadID, ok := pathID(r)
	if !ok {
		s.handleNotFound(w, r)
		return
	}
	var parentID int64
	if raw := r.PostFormValue("parent"); raw != "" {
		var err error
		if parentID, err = strconv.ParseInt(raw, 10, 64); err != nil || parentID < 1 {
			s.handleNotFound(w, r)
			return
		}
	}
	body := r.PostFormValue("body")
	id, err := s.store.CreateReply(r.Context(), member(r).ID, threadID, parentID, body)
	var invalid forum.ValidationError
	if errors.As(err, &invalid) {
		s.renderThread(w, r, http.StatusUnprocessableEntity, threadID,
			replyDraft{ParentID: parentID, Body: body, Error: invalid.Message})
		return
	}
	if err != nil {
		s.refuse(w, r, err)
		return
	}
	s.logger.Info("reply created", "member_id", member(r).ID, "thread_id", threadID, "reply_id", id)
	http.Redirect(w, r, forum.ReplyPath(threadID, id), http.StatusSeeOther)
}

// editView is the edit form for a thread (title, body, tags) or a reply
// (body only).
type editView struct {
	Page
	IsThread   bool
	Action     string
	CancelURL  string
	TitleValue string
	BodyValue  string
	Tags       []tagChoice
	// Error is the validation message to show; ErrorField names the input
	// it belongs to ("title", "body", "tags").
	Error       string
	ErrorField  string
	MaxTitleLen int
}

// ownThread loads a live thread for its author to edit. On any refusal
// it has already written the response and returns false.
func (s *Server) ownThread(w http.ResponseWriter, r *http.Request) (forum.Thread, bool) {
	id, ok := pathID(r)
	if !ok {
		s.handleNotFound(w, r)
		return forum.Thread{}, false
	}
	t, err := s.store.GetThread(r.Context(), id, member(r).ID)
	switch {
	case err == nil && t.DeletedAt != nil:
		err = forum.ErrDeleted
	case err == nil && t.AuthorID != member(r).ID:
		err = forum.ErrForbidden
	}
	if err != nil {
		s.refuse(w, r, err)
		return forum.Thread{}, false
	}
	return t, true
}

// renderThreadEdit shows a thread's edit form. picked is the tag set to
// tick: the thread's own, or what a refused submit carried.
func (s *Server) renderThreadEdit(w http.ResponseWriter, r *http.Request, status int, t forum.Thread, picked []string, view editView) {
	active, err := s.store.ActiveTags(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	// A retired tag the thread still carries stays on offer for it.
	for _, slug := range t.Tags {
		if !slices.Contains(active, slug) {
			active = append(active, slug)
		}
	}
	for _, slug := range active {
		view.Tags = append(view.Tags, tagChoice{Slug: slug, Checked: slices.Contains(picked, slug)})
	}
	view.Page = s.page(r, "Edit thread", "")
	view.IsThread = true
	view.Action = forum.ThreadPath(t.ID) + "/edit"
	view.CancelURL = forum.ThreadPath(t.ID)
	view.MaxTitleLen = forum.MaxTitleLen
	s.render(w, r, status, "edit", view)
}

func (s *Server) handleThreadEdit(w http.ResponseWriter, r *http.Request) {
	t, ok := s.ownThread(w, r)
	if !ok {
		return
	}
	s.renderThreadEdit(w, r, http.StatusOK, t, t.Tags, editView{TitleValue: t.Title, BodyValue: t.BodyMD})
}

func (s *Server) handleThreadUpdate(w http.ResponseWriter, r *http.Request) {
	t, ok := s.ownThread(w, r)
	if !ok {
		return
	}
	title, body, tags := r.PostFormValue("title"), r.PostFormValue("body"), r.PostForm["tags"]
	err := s.store.EditThread(r.Context(), member(r).ID, t.ID, title, body, tags)
	var invalid forum.ValidationError
	if errors.As(err, &invalid) {
		s.renderThreadEdit(w, r, http.StatusUnprocessableEntity, t, tags, editView{
			TitleValue: title, BodyValue: body, Error: invalid.Message, ErrorField: invalid.Field,
		})
		return
	}
	if err != nil {
		s.refuse(w, r, err)
		return
	}
	http.Redirect(w, r, forum.ThreadPath(t.ID), http.StatusSeeOther)
}

// ownReply loads a live reply for its author to edit. On any refusal it
// has already written the response and returns false.
func (s *Server) ownReply(w http.ResponseWriter, r *http.Request) (forum.Reply, bool) {
	id, ok := pathID(r)
	if !ok {
		s.handleNotFound(w, r)
		return forum.Reply{}, false
	}
	reply, err := s.store.GetReply(r.Context(), id)
	if err == nil && reply.AuthorID != member(r).ID {
		err = forum.ErrForbidden
	}
	if err != nil {
		s.refuse(w, r, err)
		return forum.Reply{}, false
	}
	return reply, true
}

func (s *Server) renderReplyEdit(w http.ResponseWriter, r *http.Request, status int, reply forum.Reply, view editView) {
	view.Page = s.page(r, "Edit reply", "")
	view.Action = "/replies/" + strconv.FormatInt(reply.ID, 10) + "/edit"
	view.CancelURL = forum.ReplyPath(reply.ThreadID, reply.ID)
	s.render(w, r, status, "edit", view)
}

func (s *Server) handleReplyEdit(w http.ResponseWriter, r *http.Request) {
	reply, ok := s.ownReply(w, r)
	if !ok {
		return
	}
	s.renderReplyEdit(w, r, http.StatusOK, reply, editView{BodyValue: reply.BodyMD})
}

func (s *Server) handleReplyUpdate(w http.ResponseWriter, r *http.Request) {
	reply, ok := s.ownReply(w, r)
	if !ok {
		return
	}
	body := r.PostFormValue("body")
	threadID, err := s.store.EditReply(r.Context(), member(r).ID, reply.ID, body)
	var invalid forum.ValidationError
	if errors.As(err, &invalid) {
		s.renderReplyEdit(w, r, http.StatusUnprocessableEntity, reply, editView{
			BodyValue: body, Error: invalid.Message, ErrorField: invalid.Field,
		})
		return
	}
	if err != nil {
		s.refuse(w, r, err)
		return
	}
	http.Redirect(w, r, forum.ReplyPath(threadID, reply.ID), http.StatusSeeOther)
}
