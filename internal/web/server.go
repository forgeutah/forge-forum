// Package web serves the forum's pages: server-rendered HTML, plain form
// posts with redirect-after-post, and embedded static assets. There is no
// client-side JavaScript.
//
// Adding a page: write templates/<name>.html defining a "main" template,
// give it a view struct that embeds Page (fill it with s.page), register
// the route in routes, and call s.render. Every route sits behind the
// identity middleware and, for anything but GET and HEAD, the CSRF check.
package web

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"

	"github.com/forgeutah/forge-forum/internal/config"
	"github.com/forgeutah/forge-forum/internal/db"
	"github.com/forgeutah/forge-forum/internal/forum"
	"github.com/forgeutah/forge-forum/internal/identity"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed assets
var assetFS embed.FS

const (
	// csrfField is the form field every state-changing form carries. The
	// "csrf" template in layout.html renders it.
	csrfField = "csrf_token"
	// maxFormBytes bounds a posted form. The largest legitimate one is a
	// thread body of forum.MaxBodyLen characters.
	maxFormBytes = 1 << 20

	contentSecurityPolicy = "default-src 'none'; script-src 'self'; " +
		"style-src 'self' https://fonts.googleapis.com; font-src https://fonts.gstatic.com; " +
		"img-src 'self' https:; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"
)

// Server holds what the handlers need.
type Server struct {
	cfg      config.Config
	store    *forum.Store
	logger   *slog.Logger
	identity func(http.Handler) http.Handler
	pages    map[string]*template.Template
	assets   http.Handler
}

// New builds the server and parses its templates.
func New(cfg config.Config, database *db.DB, store *forum.Store, logger *slog.Logger) (*Server, error) {
	if len(cfg.ProxySecrets) == 0 {
		return nil, fmt.Errorf("web: no proxy secret configured")
	}
	s := &Server{
		cfg:      cfg,
		store:    store,
		logger:   logger,
		identity: identity.Middleware(cfg, database, logger),
		pages:    map[string]*template.Template{},
	}

	names, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		if path.Base(name) == "layout.html" {
			continue
		}
		t, err := template.New("layout.html").Funcs(s.funcs()).ParseFS(templateFS, "templates/layout.html", name)
		if err != nil {
			return nil, fmt.Errorf("web: parse %s: %w", name, err)
		}
		s.pages[strings.TrimSuffix(path.Base(name), ".html")] = t
	}

	sub, err := fs.Sub(assetFS, "assets")
	if err != nil {
		return nil, err
	}
	s.assets = http.StripPrefix("/assets/", http.FileServerFS(sub))
	return s, nil
}

// Handler returns the forum's handler: the proxy contract first, so that
// nothing at all is served without it, then headers, CSRF, and the routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.routes(mux)
	return s.identity(securityHeaders(s.requireCSRF(mux)))
}

func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", s.handleFeed)
	mux.HandleFunc("GET /threads/new", s.handleThreadNew)
	mux.HandleFunc("POST /threads", s.handleThreadCreate)
	mux.HandleFunc("GET /threads/{id}", s.handleThread)
	mux.HandleFunc("POST /threads/{id}/vote", s.handleVote(forum.PostThread))
	mux.HandleFunc("POST /threads/{id}/replies", s.handleReplyCreate)
	mux.HandleFunc("GET /threads/{id}/edit", s.handleThreadEdit)
	mux.HandleFunc("POST /threads/{id}/edit", s.handleThreadUpdate)
	mux.HandleFunc("POST /threads/{id}/delete", s.handleDelete(forum.PostThread))
	mux.HandleFunc("POST /replies/{id}/vote", s.handleVote(forum.PostReply))
	mux.HandleFunc("GET /replies/{id}/edit", s.handleReplyEdit)
	mux.HandleFunc("POST /replies/{id}/edit", s.handleReplyUpdate)
	mux.HandleFunc("POST /replies/{id}/delete", s.handleDelete(forum.PostReply))
	mux.HandleFunc("GET /settings", s.handleSettings)
	mux.HandleFunc("POST /settings", s.handleSettingsSave)
	mux.HandleFunc("GET /admin/tags", s.handleAdminTags)
	mux.HandleFunc("POST /admin/tags", s.handleAdminTagAdd)
	mux.HandleFunc("POST /admin/tags/{slug}/toggle", s.handleAdminTagToggle)
	mux.HandleFunc("GET /assets/{file}", s.handleAsset)
	mux.HandleFunc("/", s.handleNotFound)
}

// Page is the data every template needs for the layout. View structs embed
// it; build it with s.page.
type Page struct {
	// Title is the document title, before the site name.
	Title string
	// Nav names the nav link to mark current: "feed", "settings", "admin",
	// or "".
	Nav    string
	Member identity.Member
	// CSRF is the member's token for the "csrf" template.
	CSRF string
	// SignOutURL is empty when no auth portal is configured.
	SignOutURL string
	// HideNewThread drops the nav's "New thread" button, as on the
	// new-thread page itself.
	HideNewThread bool
}

// page fills the layout data for the signed-in member.
func (s *Server) page(r *http.Request, title, nav string) Page {
	m := member(r)
	return Page{Title: title, Nav: nav, Member: m, CSRF: s.csrfToken(m), SignOutURL: s.cfg.AuthPortalURL}
}

// member returns the signed-in member. The identity middleware guarantees
// one for every request that reaches a handler.
func member(r *http.Request) identity.Member {
	m, _ := identity.FromContext(r.Context())
	return m
}

// render executes a page template into a buffer first, so a template error
// never leaves a half-written page.
func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, name string, view any) {
	t, ok := s.pages[name]
	if !ok {
		s.serverError(w, r, fmt.Errorf("no template %q", name))
		return
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, view); err != nil {
		s.serverError(w, r, fmt.Errorf("render %s: %w", name, err))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Pages are per-member and members-only; never let a shared cache keep one.
	w.Header().Set("Cache-Control", "private, no-cache")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

type messageView struct {
	Page
	Heading string
	Message string
}

// renderMessage shows a short notice page: not found, removed, forbidden.
func (s *Server) renderMessage(w http.ResponseWriter, r *http.Request, status int, heading, message string) {
	s.render(w, r, status, "message", messageView{Page: s.page(r, heading, ""), Heading: heading, Message: message})
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	s.renderMessage(w, r, http.StatusNotFound, "Page not found", "Nothing lives at this address. It may have moved, or the link may be wrong.")
}

// serverError logs err against the member and shows a plain apology.
func (s *Server) serverError(w http.ResponseWriter, r *http.Request, err error) {
	s.logger.Error("request failed", "error", err, "member_id", member(r).ID, "method", r.Method, "path", r.URL.Path)
	http.Error(w, "Something went wrong on our side. Try again in a moment.", http.StatusInternalServerError)
}

func (s *Server) handleAsset(w http.ResponseWriter, r *http.Request) {
	// Assets are not fingerprinted, so keep the cache short.
	w.Header().Set("Cache-Control", "private, max-age=300")
	s.assets.ServeHTTP(w, r)
}

// securityHeaders sets the headers every response carries. The policy has
// no inline allowance: pages load only their own stylesheets, Google Fonts,
// and https images (member avatars and images in posts).
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// csrfToken returns the member's form token: an HMAC of their Slack user
// ID under the proxy secret. It needs no server-side state, and one
// member's token is useless for another.
func (s *Server) csrfToken(m identity.Member) string {
	mac := hmac.New(sha256.New, []byte(s.cfg.ProxySecrets[0]))
	mac.Write([]byte("forge-forum csrf v1\x00" + m.SlackUserID))
	return hex.EncodeToString(mac.Sum(nil))
}

// checkCSRF reports whether the request's form carries the signed-in
// member's token. It parses the form.
func (s *Server) checkCSRF(r *http.Request) bool {
	m, ok := identity.FromContext(r.Context())
	if !ok || m.SlackUserID == "" {
		return false
	}
	got := r.PostFormValue(csrfField)
	return hmac.Equal([]byte(got), []byte(s.csrfToken(m)))
}

// requireCSRF rejects any state-changing request without a valid token, so
// a new POST route is protected without its handler doing anything.
func (s *Server) requireCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
			if !s.checkCSRF(r) {
				s.logger.Warn("csrf check failed", "member_id", member(r).ID, "path", r.URL.Path)
				s.renderMessage(w, r, http.StatusForbidden, "That form expired",
					"We could not confirm the form came from this site. Go back, reload the page, and try again.")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
