package web

import (
	"errors"
	"html/template"
	"net/http"
	"strconv"

	"github.com/forgeutah/forge-forum/internal/forum"
	"github.com/forgeutah/forge-forum/internal/markdown"
)

type threadView struct {
	Page
	Thread forum.Thread
	// Body is the thread's Markdown, rendered and sanitized.
	Body template.HTML
}

// handleThread shows one thread. It is deliberately small: the reply tree,
// voting, and edit controls are added to this handler and thread.html.
func (s *Server) handleThread(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		s.handleNotFound(w, r)
		return
	}
	t, err := s.store.GetThread(r.Context(), id, member(r).ID)
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
	s.render(w, r, http.StatusOK, "thread", threadView{
		Page:   s.page(r, t.Title, ""),
		Thread: t,
		Body:   markdown.Render(t.BodyMD),
	})
}
