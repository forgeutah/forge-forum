package web

import "net/http"

type settingsView struct {
	Page
	// SlackOn is false when the forum has no Slack connection, so the
	// setting would do nothing.
	SlackOn   bool
	DMReplies bool
	Saved     bool
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	on, err := s.store.DMReplies(r.Context(), member(r).ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "settings", settingsView{
		Page:      s.page(r, "Settings", "settings"),
		SlackOn:   s.cfg.SlackEnabled(),
		DMReplies: on,
		Saved:     r.URL.Query().Get("saved") == "1",
	})
}

func (s *Server) handleSettingsSave(w http.ResponseWriter, r *http.Request) {
	// An unticked checkbox is simply absent from the form.
	if err := s.store.SetDMReplies(r.Context(), member(r).ID, r.PostFormValue("dm_replies") != ""); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/settings?saved=1", http.StatusSeeOther)
}
