package web

import (
	"errors"
	"net/http"

	"github.com/forgeutah/forge-forum/internal/forum"
)

type adminTagsView struct {
	Page
	Tags      []forum.Tag
	SlugValue string
	Error     string
}

// renderAdminTags shows the tag list to an admin and a 403 to anyone
// else. The store checks the role again on every change.
func (s *Server) renderAdminTags(w http.ResponseWriter, r *http.Request, status int, view adminTagsView) {
	if !member(r).IsAdmin {
		s.refuse(w, r, forum.ErrForbidden)
		return
	}
	tags, err := s.store.AllTags(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	view.Page = s.page(r, "Manage tags", "admin")
	view.Tags = tags
	s.render(w, r, status, "admin_tags", view)
}

func (s *Server) handleAdminTags(w http.ResponseWriter, r *http.Request) {
	s.renderAdminTags(w, r, http.StatusOK, adminTagsView{})
}

func (s *Server) handleAdminTagAdd(w http.ResponseWriter, r *http.Request) {
	slug := r.PostFormValue("slug")
	err := s.store.AddTag(r.Context(), actor(r), slug)
	var invalid forum.ValidationError
	if errors.As(err, &invalid) {
		s.renderAdminTags(w, r, http.StatusUnprocessableEntity, adminTagsView{SlugValue: slug, Error: invalid.Message})
		return
	}
	if err != nil {
		s.refuse(w, r, err)
		return
	}
	s.logger.Info("tag added", "member_id", member(r).ID, "slug", slug)
	http.Redirect(w, r, "/admin/tags", http.StatusSeeOther)
}

// handleAdminTagToggle retires a tag or brings it back. The form names
// the state wanted ("active" is "1" or "0"), so a repeated submit is
// harmless.
func (s *Server) handleAdminTagToggle(w http.ResponseWriter, r *http.Request) {
	slug, active := r.PathValue("slug"), r.PostFormValue("active") == "1"
	if err := s.store.SetTagActive(r.Context(), actor(r), slug, active); err != nil {
		s.refuse(w, r, err)
		return
	}
	s.logger.Info("tag toggled", "member_id", member(r).ID, "slug", slug, "active", active)
	http.Redirect(w, r, "/admin/tags", http.StatusSeeOther)
}
