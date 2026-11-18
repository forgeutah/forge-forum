package main

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/forgeutah/forge-forum/internal/config"
	"github.com/forgeutah/forge-forum/internal/db"
)

const buildTestSecret = "build-test-secret-build-test-secret"

func signed(method, path string, form url.Values) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	r.Header.Set("X-Forge-Proxy-Secret", buildTestSecret)
	r.Header.Set("X-Forge-Contract-Version", "1")
	r.Header.Set("X-Forge-User-Id", "7")
	r.Header.Set("X-Forge-Name", "Sam R")
	r.Header.Set("X-Forge-Slack-User-Id", "U0SAM")
	r.Header.Set("X-Forge-Slack-Team-Id", "T0FORGE")
	return r
}

// TestBuild_SlackOff_PostSucceedsAndOutboxRowIsSkipped checks the wiring
// with Slack unconfigured: it is logged once, a thread posts, and the
// started worker is woken and marks the announcement skipped.
func TestBuild_SlackOff_PostSucceedsAndOutboxRowIsSkipped(t *testing.T) {
	database, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "forum.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	var logs bytes.Buffer
	cfg := config.Config{
		ProxySecrets:  []string{buildTestSecret},
		SlackTeamID:   "T0FORGE",
		AdminRole:     "admin",
		PublicBaseURL: "https://forum.example.com",
	}
	handler, start, err := build(ctx, cfg, database, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if n := strings.Count(logs.String(), "Slack is not configured"); n != 1 {
		t.Errorf("Slack-off notice logged %d times, want once", n)
	}
	start()

	do := func(r *http.Request) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if w := do(httptest.NewRequest(http.MethodGet, "/healthz", nil)); w.Code != http.StatusOK {
		t.Errorf("GET /healthz: status %d", w.Code)
	}
	if w := do(httptest.NewRequest(http.MethodGet, "/", nil)); w.Code != http.StatusUnauthorized {
		t.Errorf("GET / without the proxy contract: status %d, want 401", w.Code)
	}

	token := regexp.MustCompile(`name="csrf_token" value="([^"]+)"`).FindStringSubmatch(do(signed(http.MethodGet, "/threads/new", nil)).Body.String())
	if token == nil {
		t.Fatal("no CSRF token on the new-thread form")
	}
	w := do(signed(http.MethodPost, "/threads", url.Values{
		"csrf_token": {token[1]}, "title": {"Hello"}, "body": {"Body"}, "tags": {"general"},
	}))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("POST /threads: status %d", w.Code)
	}

	// The post woke the worker; it should settle the row well inside its
	// polling interval.
	deadline := time.Now().Add(3 * time.Second)
	var kind, status string
	for {
		err := database.Reader().QueryRow(`SELECT kind, status FROM notifications`).Scan(&kind, &status)
		if err == nil && status != "pending" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("outbox row not settled: kind %q status %q err %v", kind, status, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if kind != "announce" || status != "skipped" {
		t.Errorf("outbox row = %s/%s, want announce/skipped", kind, status)
	}
}
