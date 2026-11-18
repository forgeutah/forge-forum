package web

import (
	"errors"
	"net/http"
	"slices"

	"github.com/forgeutah/forge-forum/internal/forum"
)

type tagChoice struct {
	Slug    string
	Checked bool
}

type threadNewView struct {
	Page
	TitleValue string
	BodyValue  string
	Tags       []tagChoice
	// Error is the validation message to show; ErrorField names the input
	// it belongs to ("title", "body", "tags").
	Error      string
	ErrorField string
	// AnnouncesInSlack is true when posting will announce the thread.
	AnnouncesInSlack bool
	MaxTitleLen      int
}

func (s *Server) handleThreadNew(w http.ResponseWriter, r *http.Request) {
	s.renderThreadNew(w, r, http.StatusOK, threadNewView{})
}

// renderThreadNew shows the form, keeping whatever the member typed.
func (s *Server) renderThreadNew(w http.ResponseWriter, r *http.Request, status int, view threadNewView) {
	active, err := s.store.ActiveTags(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	picked := r.PostForm["tags"]
	for _, slug := range active {
		view.Tags = append(view.Tags, tagChoice{Slug: slug, Checked: slices.Contains(picked, slug)})
	}
	view.Page = s.page(r, "New thread", "")
	view.HideNewThread = true
	view.AnnouncesInSlack = s.cfg.SlackEnabled() && s.cfg.SlackAnnounceChannel != ""
	view.MaxTitleLen = forum.MaxTitleLen
	s.render(w, r, status, "thread_new", view)
}

func (s *Server) handleThreadCreate(w http.ResponseWriter, r *http.Request) {
	// requireCSRF has already parsed the form.
	title, body := r.PostFormValue("title"), r.PostFormValue("body")
	id, err := s.store.CreateThread(r.Context(), member(r).ID, title, body, r.PostForm["tags"])
	var invalid forum.ValidationError
	if errors.As(err, &invalid) {
		s.renderThreadNew(w, r, http.StatusUnprocessableEntity, threadNewView{
			TitleValue: title, BodyValue: body, Error: invalid.Message, ErrorField: invalid.Field,
		})
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.logger.Info("thread created", "member_id", member(r).ID, "thread_id", id)
	http.Redirect(w, r, threadURL(id), http.StatusSeeOther)
}
