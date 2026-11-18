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
	"time"

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
		first.AvatarURL != "https://avatars.example.com/sam.png" {
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
		importedID, err = EnsureBySlackID(ctx, tx, "U0SAM", "sam (slack)", "", time.Unix(1_700_000_000, 0))
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

	created := time.Unix(1_700_000_000, 0)
	var id, newID int64
	err := d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		var err error
		if id, err = EnsureBySlackID(ctx, tx, "U0SAM", "other name", "https://x.example/a.png", created); err != nil {
			return err
		}
		newID, err = EnsureBySlackID(ctx, tx, "U0NEW", "New Person", "https://x.example/n.png", created)
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
	var createdAt int64
	d.Reader().QueryRow(`SELECT name, avatar_url FROM members WHERE id = ?`, id).Scan(&name, &avatar)
	if name != "Sam R" || avatar != "https://avatars.example.com/sam.png" {
		t.Errorf("existing member overwritten: %q %q", name, avatar)
	}
	d.Reader().QueryRow(`SELECT name, forge_user_id, created_at FROM members WHERE id = ?`, newID).Scan(&name, &forgeID, &createdAt)
	if name != "New Person" || forgeID.Valid {
		t.Errorf("new member = %q forge_user_id = %v, want New Person and NULL", name, forgeID)
	}
	if createdAt != created.Unix() {
		t.Errorf("new member created_at = %d, want the passed clock %d", createdAt, created.Unix())
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

// setClock makes the middleware read a clock the test controls and
// returns the function that moves it forward.
func setClock(t *testing.T, start time.Time) (advance func(time.Duration)) {
	t.Helper()
	current, real := start, now
	now = func() time.Time { return current }
	t.Cleanup(func() { now = real })
	return func(d time.Duration) { current = current.Add(d) }
}

// storedMember is the members row for U0SAM as the database holds it.
type storedMember struct {
	ID       int64
	ForgeID  sql.NullInt64
	Name     string
	Avatar   string
	LastSeen sql.NullInt64
}

func readSam(t *testing.T, d *db.DB) storedMember {
	t.Helper()
	var s storedMember
	err := d.Reader().QueryRow(`
		SELECT id, forge_user_id, name, avatar_url, last_seen_at FROM members WHERE slack_user_id = 'U0SAM'`).
		Scan(&s.ID, &s.ForgeID, &s.Name, &s.Avatar, &s.LastSeen)
	if err != nil {
		t.Fatalf("read member: %v", err)
	}
	return s
}

// TestMiddleware_SecondRequestWithinWindow_WritesNothing checks that an
// unchanged member seen under a minute ago is served without a write.
func TestMiddleware_SecondRequestWithinWindow_WritesNothing(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	advance := setClock(t, start)
	d := openTestDB(t)
	var first, second Member
	serve(testHandler(t, d, &first), proxyRequest(nil))
	if got := readSam(t, d).LastSeen; !got.Valid || got.Int64 != start.Unix() {
		t.Fatalf("last_seen_at after first request = %v, want %d", got, start.Unix())
	}
	// A write would show up here even if it left every column the
	// middleware sets untouched.
	var changesBefore, changesAfter int64
	countWrites := func(n *int64) error {
		return d.WithWriteTx(context.Background(), func(tx *sql.Tx) error {
			return tx.QueryRow(`SELECT total_changes()`).Scan(n)
		})
	}
	if err := countWrites(&changesBefore); err != nil {
		t.Fatalf("total_changes: %v", err)
	}

	advance(59 * time.Second)
	w := serve(testHandler(t, d, &second), proxyRequest(map[string]string{"X-Forge-Roles": "admin"}))
	if w.Code != http.StatusOK || w.Body.String() != pageContent {
		t.Fatalf("second request status = %d body = %q", w.Code, w.Body.String())
	}
	if err := countWrites(&changesAfter); err != nil {
		t.Fatalf("total_changes: %v", err)
	}
	if changesAfter != changesBefore {
		t.Errorf("second request changed %d rows, want none", changesAfter-changesBefore)
	}
	if got := readSam(t, d).LastSeen; got.Int64 != start.Unix() {
		t.Errorf("last_seen_at = %d, want it left at %d", got.Int64, start.Unix())
	}
	want := first
	want.IsAdmin = true
	if second != want {
		t.Errorf("second member = %+v, want %+v", second, want)
	}
	if n := countMembers(t, d); n != 1 {
		t.Errorf("members = %d, want 1", n)
	}
}

// TestMiddleware_ChangedProfileWithinWindow_UpdatesAtOnce checks that a
// new name, avatar or Forge user ID is stored without waiting a minute.
func TestMiddleware_ChangedProfileWithinWindow_UpdatesAtOnce(t *testing.T) {
	cases := map[string]struct {
		override map[string]string
		want     storedMember
	}{
		"name": {
			map[string]string{"X-Forge-Name": "Sam Rivera"},
			storedMember{Name: "Sam Rivera", Avatar: "https://avatars.example.com/sam.png", ForgeID: sql.NullInt64{Int64: 42, Valid: true}},
		},
		"avatar": {
			map[string]string{"X-Forge-Avatar": "https://avatars.example.com/sam2.png"},
			storedMember{Name: "Sam R", Avatar: "https://avatars.example.com/sam2.png", ForgeID: sql.NullInt64{Int64: 42, Valid: true}},
		},
		"avatar removed": {
			map[string]string{"X-Forge-Avatar": ""},
			storedMember{Name: "Sam R", Avatar: "", ForgeID: sql.NullInt64{Int64: 42, Valid: true}},
		},
		"forge user id": {
			map[string]string{"X-Forge-User-Id": "43"},
			storedMember{Name: "Sam R", Avatar: "https://avatars.example.com/sam.png", ForgeID: sql.NullInt64{Int64: 43, Valid: true}},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			start := time.Unix(1_800_000_000, 0)
			advance := setClock(t, start)
			d := openTestDB(t)
			var first, second Member
			serve(testHandler(t, d, &first), proxyRequest(nil))

			advance(5 * time.Second)
			serve(testHandler(t, d, &second), proxyRequest(tc.override))
			got := readSam(t, d)
			tc.want.ID = first.ID
			tc.want.LastSeen = sql.NullInt64{Int64: start.Unix() + 5, Valid: true}
			if got != tc.want {
				t.Errorf("stored member = %+v, want %+v", got, tc.want)
			}
			if second.ID != first.ID || second.Name != tc.want.Name || second.AvatarURL != tc.want.Avatar {
				t.Errorf("request member = %+v, want the stored row %+v", second, tc.want)
			}
		})
	}
}

// TestMiddleware_RequestAfterWindow_RefreshesLastSeen checks that
// last_seen_at moves once the stored value is a minute old.
func TestMiddleware_RequestAfterWindow_RefreshesLastSeen(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	advance := setClock(t, start)
	d := openTestDB(t)
	var first, second Member
	serve(testHandler(t, d, &first), proxyRequest(nil))

	advance(60 * time.Second)
	serve(testHandler(t, d, &second), proxyRequest(nil))
	if got := readSam(t, d).LastSeen; got.Int64 != start.Unix()+60 {
		t.Errorf("last_seen_at = %d, want it refreshed to %d", got.Int64, start.Unix()+60)
	}
	if second != first {
		t.Errorf("second member = %+v, want %+v", second, first)
	}

	// The refresh starts a new window.
	advance(30 * time.Second)
	serve(testHandler(t, d, nil), proxyRequest(nil))
	if got := readSam(t, d).LastSeen; got.Int64 != start.Unix()+60 {
		t.Errorf("last_seen_at = %d, want it left at %d inside the new window", got.Int64, start.Unix()+60)
	}
}

// TestMiddleware_ImportedMemberFirstSignIn_SetsForgeUserID checks that a
// row an import created, which has no Forge user ID and was never seen,
// is written on its owner's first request.
func TestMiddleware_ImportedMemberFirstSignIn_SetsForgeUserID(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	setClock(t, start)
	ctx := context.Background()
	d := openTestDB(t)
	var importedID int64
	err := d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		var err error
		// The import already holds the name and avatar the proxy will send.
		importedID, err = EnsureBySlackID(ctx, tx, "U0SAM", "Sam R", "https://avatars.example.com/sam.png", start)
		return err
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if got := readSam(t, d); got.ForgeID.Valid || got.LastSeen.Valid {
		t.Fatalf("imported row = %+v, want no forge_user_id and no last_seen_at", got)
	}

	var seen Member
	serve(testHandler(t, d, &seen), proxyRequest(nil))
	got := readSam(t, d)
	if seen.ID != importedID || got.ID != importedID {
		t.Errorf("member ID = %d (row %d), want the imported row %d", seen.ID, got.ID, importedID)
	}
	if !got.ForgeID.Valid || got.ForgeID.Int64 != 42 || got.LastSeen.Int64 != start.Unix() {
		t.Errorf("stored member = %+v, want forge_user_id 42 and last_seen_at %d", got, start.Unix())
	}
	if n := countMembers(t, d); n != 1 {
		t.Errorf("members = %d, want 1", n)
	}
}
