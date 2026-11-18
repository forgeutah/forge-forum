package identity

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/forgeutah/forge-forum/internal/config"
	"github.com/forgeutah/forge-forum/internal/db"
)

const (
	testSecret    = "current-secret-current-secret-current"
	testOldSecret = "previous-secret-previous-secret-prev"
	testTeam      = "T0FORGE"
	pageContent   = "members-only page content"
)

func testConfig() config.Config {
	return config.Config{
		ProxySecrets: []string{testSecret, testOldSecret},
		SlackTeamID:  testTeam,
		AdminRole:    "admin",
	}
}

func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "forum.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// testHandler returns the middleware wrapped around a handler that writes
// pageContent and records the member it saw.
func testHandler(t *testing.T, d *db.DB, seen *Member) http.Handler {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return Middleware(testConfig(), d, logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m, ok := FromContext(r.Context())
		if !ok {
			t.Error("handler ran without a member in the context")
		}
		if seen != nil {
			*seen = m
		}
		io.WriteString(w, pageContent)
	}))
}

// proxyRequest builds a request carrying a complete, valid proxy contract.
// Entries in override replace headers; an empty value removes the header.
func proxyRequest(override map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/threads/1", nil)
	headers := map[string]string{
		"X-Forge-Proxy-Secret":     testSecret,
		"X-Forge-Contract-Version": "1",
		"X-Forge-User-Id":          "42",
		"X-Forge-Email":            "sam@example.com",
		"X-Forge-Name":             "Sam R",
		"X-Forge-Avatar":           "https://avatars.example.com/sam.png",
		"X-Forge-Roles":            "",
		"X-Forge-Slack-User-Id":    "U0SAM",
		"X-Forge-Slack-Team-Id":    testTeam,
	}
	for k, v := range override {
		headers[k] = v
	}
	for k, v := range headers {
		if v != "" {
			r.Header.Set(k, v)
		}
	}
	return r
}

func serve(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func countMembers(t *testing.T, d *db.DB) int {
	t.Helper()
	var n int
	if err := d.Reader().QueryRow(`SELECT COUNT(*) FROM members`).Scan(&n); err != nil {
		t.Fatalf("count members: %v", err)
	}
	return n
}

// TestMiddleware_BrokenContract_Returns401WithoutContent covers AE1: a
// request that fails any part of the proxy contract sees no forum content
// and creates no member.
func TestMiddleware_BrokenContract_Returns401WithoutContent(t *testing.T) {
	cases := map[string]map[string]string{
		"no secret":            {"X-Forge-Proxy-Secret": ""},
		"wrong secret":         {"X-Forge-Proxy-Secret": "not-the-secret-not-the-secret-not-the"},
		"secret prefix":        {"X-Forge-Proxy-Secret": testSecret[:10]},
		"wrong team":           {"X-Forge-Slack-Team-Id": "T0OTHER"},
		"no team":              {"X-Forge-Slack-Team-Id": ""},
		"no slack user":        {"X-Forge-Slack-User-Id": ""},
		"unknown contract":     {"X-Forge-Contract-Version": "2"},
		"non-numeric user id":  {"X-Forge-User-Id": "abc"},
		"missing user id":      {"X-Forge-User-Id": ""},
		"right team, no creds": {"X-Forge-Proxy-Secret": "", "X-Forge-Slack-User-Id": ""},
	}
	for name, override := range cases {
		t.Run(name, func(t *testing.T) {
			d := openTestDB(t)
			w := serve(testHandler(t, d, nil), proxyRequest(override))
			if w.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", w.Code)
			}
			if strings.Contains(w.Body.String(), pageContent) {
				t.Error("response leaked page content")
			}
			if n := countMembers(t, d); n != 0 {
				t.Errorf("members = %d, want 0", n)
			}
		})
	}
}

// TestMiddleware_NoHeadersAtAll_Returns401 checks a direct request that
// bypassed forge-proxy entirely.
func TestMiddleware_NoHeadersAtAll_Returns401(t *testing.T) {
	d := openTestDB(t)
	w := serve(testHandler(t, d, nil), httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusUnauthorized || strings.Contains(w.Body.String(), pageContent) {
		t.Errorf("status = %d body = %q, want 401 and no content", w.Code, w.Body.String())
	}
}

// TestMiddleware_PreviousSecret_Accepted checks that the second entry of a
// two-secret list still passes during a rotation.
func TestMiddleware_PreviousSecret_Accepted(t *testing.T) {
	d := openTestDB(t)
	w := serve(testHandler(t, d, nil), proxyRequest(map[string]string{"X-Forge-Proxy-Secret": testOldSecret}))
	if w.Code != http.StatusOK || w.Body.String() != pageContent {
		t.Errorf("status = %d body = %q, want 200 and the page", w.Code, w.Body.String())
	}
}

// TestMiddleware_AbsentContractVersion_TreatedAsOne checks that a proxy
// that sends no version header is accepted.
func TestMiddleware_AbsentContractVersion_TreatedAsOne(t *testing.T) {
	d := openTestDB(t)
	w := serve(testHandler(t, d, nil), proxyRequest(map[string]string{"X-Forge-Contract-Version": ""}))
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

// TestMiddleware_FirstAndSecondRequest_UpsertOneMember checks that the
// first request creates a member and the second refreshes name and avatar
// on the same row.
func TestMiddleware_FirstAndSecondRequest_UpsertOneMember(t *testing.T) {
	d := openTestDB(t)
	var first, second Member
	if w := serve(testHandler(t, d, &first), proxyRequest(nil)); w.Code != http.StatusOK {
		t.Fatalf("first request status = %d", w.Code)
	}
	if first.ID == 0 || first.Name != "Sam R" || first.SlackUserID != "U0SAM" ||
		first.AvatarURL != "https://avatars.example.com/sam.png" || !first.DMReplies {
		t.Errorf("first member = %+v", first)
	}

	serve(testHandler(t, d, &second), proxyRequest(map[string]string{
		"X-Forge-Name":   "Sam Rivera",
		"X-Forge-Avatar": "https://avatars.example.com/sam2.png",
	}))
	if second.ID != first.ID {
		t.Errorf("second request member ID = %d, want %d", second.ID, first.ID)
	}
	if n := countMembers(t, d); n != 1 {
		t.Errorf("members = %d, want 1", n)
	}

	var name, avatar string
	var forgeID int64
	var lastSeen sql.NullInt64
	err := d.Reader().QueryRow(`SELECT name, avatar_url, forge_user_id, last_seen_at FROM members WHERE id = ?`, first.ID).
		Scan(&name, &avatar, &forgeID, &lastSeen)
	if err != nil {
		t.Fatalf("read member: %v", err)
	}
	if name != "Sam Rivera" || avatar != "https://avatars.example.com/sam2.png" || forgeID != 42 || !lastSeen.Valid {
		t.Errorf("stored member = %q %q %d %v", name, avatar, forgeID, lastSeen)
	}
}

// TestMiddleware_ImportedMemberSignsIn_GainsForgeIDKeepsPosts checks that a
// row created by an import is claimed on first sign-in, not duplicated.
func TestMiddleware_ImportedMemberSignsIn_GainsForgeIDKeepsPosts(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	var importedID int64
	err := d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		var err error
		importedID, err = EnsureBySlackID(ctx, tx, "U0SAM", "sam (slack)", "")
		if err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO threads (author_id, title, body_md, created_at, last_activity_at)
			VALUES (?, 'Imported', 'body', 1, 1)`, importedID)
		return err
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	var seen Member
	serve(testHandler(t, d, &seen), proxyRequest(nil))
	if seen.ID != importedID {
		t.Fatalf("signed-in member ID = %d, want the imported row %d", seen.ID, importedID)
	}
	var forgeID sql.NullInt64
	var name string
	var posts int
	d.Reader().QueryRow(`SELECT forge_user_id, name FROM members WHERE id = ?`, importedID).Scan(&forgeID, &name)
	d.Reader().QueryRow(`SELECT COUNT(*) FROM threads WHERE author_id = ?`, importedID).Scan(&posts)
	if !forgeID.Valid || forgeID.Int64 != 42 || name != "Sam R" || posts != 1 {
		t.Errorf("forge_user_id = %v name = %q posts = %d, want 42, Sam R, 1", forgeID, name, posts)
	}
}

// TestEnsureBySlackID_ExistingMember_KeepsNameAndAvatar checks that an
// import never overwrites what a signed-in member's row already holds.
func TestEnsureBySlackID_ExistingMember_KeepsNameAndAvatar(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	var seen Member
	serve(testHandler(t, d, &seen), proxyRequest(nil))

	var id, newID int64
	err := d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		var err error
		if id, err = EnsureBySlackID(ctx, tx, "U0SAM", "other name", "https://x.example/a.png"); err != nil {
			return err
		}
		newID, err = EnsureBySlackID(ctx, tx, "U0NEW", "New Person", "https://x.example/n.png")
		return err
	})
	if err != nil {
		t.Fatalf("EnsureBySlackID: %v", err)
	}
	if id != seen.ID {
		t.Errorf("id = %d, want existing %d", id, seen.ID)
	}
	if newID == 0 || newID == id {
		t.Errorf("new member id = %d", newID)
	}
	var name, avatar string
	var forgeID sql.NullInt64
	d.Reader().QueryRow(`SELECT name, avatar_url FROM members WHERE id = ?`, id).Scan(&name, &avatar)
	if name != "Sam R" || avatar != "https://avatars.example.com/sam.png" {
		t.Errorf("existing member overwritten: %q %q", name, avatar)
	}
	d.Reader().QueryRow(`SELECT name, forge_user_id FROM members WHERE id = ?`, newID).Scan(&name, &forgeID)
	if name != "New Person" || forgeID.Valid {
		t.Errorf("new member = %q forge_user_id = %v, want New Person and NULL", name, forgeID)
	}
}

// TestMiddleware_RFC8187Name_Decoded checks that a non-ASCII name sent in
// the extended form arrives as the accented name.
func TestMiddleware_RFC8187Name_Decoded(t *testing.T) {
	cases := map[string]string{
		"UTF-8''Jos%C3%A9%20N%C3%BA%C3%B1ez": "José Núñez",
		"utf-8'en'Zo%C3%AB":                  "Zoë",
		"Plain Name":                         "Plain Name",
		"UTF-8''bad%ZZescape":                "UTF-8''bad%ZZescape",
	}
	for header, want := range cases {
		t.Run(want, func(t *testing.T) {
			d := openTestDB(t)
			var seen Member
			serve(testHandler(t, d, &seen), proxyRequest(map[string]string{"X-Forge-Name": header}))
			if seen.Name != want {
				t.Errorf("name = %q, want %q", seen.Name, want)
			}
		})
	}
}

// TestMiddleware_EmptyName_FallsBackToSlackID checks that a member always
// has something to display.
func TestMiddleware_EmptyName_FallsBackToSlackID(t *testing.T) {
	d := openTestDB(t)
	var seen Member
	serve(testHandler(t, d, &seen), proxyRequest(map[string]string{"X-Forge-Name": ""}))
	if seen.Name != "U0SAM" {
		t.Errorf("name = %q, want the Slack user ID", seen.Name)
	}
}

// TestMiddleware_NonHTTPSAvatar_Dropped checks that only https avatar URLs
// are stored, since they are rendered as image sources.
func TestMiddleware_NonHTTPSAvatar_Dropped(t *testing.T) {
	d := openTestDB(t)
	var seen Member
	serve(testHandler(t, d, &seen), proxyRequest(map[string]string{"X-Forge-Avatar": "javascript:alert(1)"}))
	if seen.AvatarURL != "" {
		t.Errorf("avatar = %q, want empty", seen.AvatarURL)
	}
}

// TestMiddleware_Roles_SetAdminFlag checks that only the configured admin
// role grants admin, and that the flag is never stored.
func TestMiddleware_Roles_SetAdminFlag(t *testing.T) {
	cases := map[string]bool{
		"":                   false,
		"admin":              true,
		"organizer, admin":   true,
		"organizer,ADMIN":    false,
		"administrator":      false,
		"organizer,sponsors": false,
	}
	for roles, want := range cases {
		t.Run(roles, func(t *testing.T) {
			d := openTestDB(t)
			var seen Member
			serve(testHandler(t, d, &seen), proxyRequest(map[string]string{"X-Forge-Roles": roles}))
			if seen.IsAdmin != want {
				t.Errorf("IsAdmin = %v, want %v", seen.IsAdmin, want)
			}
		})
	}
}

// TestWithMember_RoundTrip checks the context helpers other packages use.
func TestWithMember_RoundTrip(t *testing.T) {
	if _, ok := FromContext(context.Background()); ok {
		t.Error("empty context reported a member")
	}
	m, ok := FromContext(WithMember(context.Background(), Member{ID: 7, Name: "x"}))
	if !ok || m.ID != 7 {
		t.Errorf("FromContext = %+v, %v", m, ok)
	}
}
