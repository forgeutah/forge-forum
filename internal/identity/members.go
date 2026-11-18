// Package identity turns forge-proxy's upstream headers into forum members.
//
// Members are keyed on Slack user ID (plan KTD3). A row may exist before
// its owner ever signs in, because moving a Slack thread credits its Slack
// authors; the Forge user ID is attached when that person first arrives.
package identity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/forgeutah/forge-forum/internal/db"
)

// lastSeenWindow is how stale last_seen_at may get before a request from
// an otherwise unchanged member writes to the database again.
const lastSeenWindow = 60 * time.Second

// Member is a forum member as seen by one request.
type Member struct {
	ID          int64
	SlackUserID string
	Name        string
	AvatarURL   string
	// IsAdmin is request-scoped: it comes from X-Forge-Roles and is never
	// stored, so revoking the role in forge-proxy takes effect at once.
	IsAdmin bool
}

type contextKey struct{}

// WithMember returns ctx carrying m.
func WithMember(ctx context.Context, m Member) context.Context {
	return context.WithValue(ctx, contextKey{}, m)
}

// FromContext returns the member the middleware attached to ctx.
func FromContext(ctx context.Context) (Member, bool) {
	m, ok := ctx.Value(contextKey{}).(Member)
	return m, ok
}

// signIn returns the stored member for a signed-in request. It writes
// only when it has to: when the member is new, when their Forge user ID,
// name or avatar changed, or when last_seen_at is older than
// lastSeenWindow. Every other request is served from the read pool.
func signIn(ctx context.Context, database *db.DB, m Member, forgeUserID int64, now time.Time) (Member, error) {
	if id, ok := freshMemberID(ctx, database, m, forgeUserID, now); ok {
		m.ID = id
		return m, nil
	}
	err := database.WithWriteTx(ctx, func(tx *sql.Tx) error {
		var err error
		m, err = upsertSignedIn(ctx, tx, m, forgeUserID, now)
		return err
	})
	return m, err
}

// freshMemberID reports the ID of the row that already holds exactly what
// this request would write and was seen within lastSeenWindow. Any doubt,
// including a failed read, reports false so the upsert decides.
func freshMemberID(ctx context.Context, database *db.DB, m Member, forgeUserID int64, now time.Time) (int64, bool) {
	var (
		id             int64
		storedForgeID  sql.NullInt64
		name, avatar   string
		storedLastSeen sql.NullInt64
	)
	err := database.Reader().QueryRowContext(ctx, `
		SELECT id, forge_user_id, name, avatar_url, last_seen_at
		FROM members WHERE slack_user_id = ?`, m.SlackUserID,
	).Scan(&id, &storedForgeID, &name, &avatar, &storedLastSeen)
	if err != nil {
		return 0, false
	}
	unchanged := storedForgeID.Valid && storedForgeID.Int64 == forgeUserID &&
		name == m.Name && avatar == m.AvatarURL
	seenRecently := storedLastSeen.Valid &&
		now.Sub(time.Unix(storedLastSeen.Int64, 0)) < lastSeenWindow
	return id, unchanged && seenRecently
}

// upsertSignedIn records a signed-in member, creating the row or
// refreshing the one already keyed on their Slack user ID.
func upsertSignedIn(ctx context.Context, tx *sql.Tx, m Member, forgeUserID int64, now time.Time) (Member, error) {
	err := tx.QueryRowContext(ctx, `
		INSERT INTO members (slack_user_id, forge_user_id, name, avatar_url, created_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (slack_user_id) DO UPDATE SET
			forge_user_id = excluded.forge_user_id,
			name          = excluded.name,
			avatar_url    = excluded.avatar_url,
			last_seen_at  = excluded.last_seen_at
		RETURNING id`,
		m.SlackUserID, forgeUserID, m.Name, m.AvatarURL, now.Unix(), now.Unix(),
	).Scan(&m.ID)
	if err != nil {
		return Member{}, fmt.Errorf("upsert member: %w", err)
	}
	return m, nil
}

// EnsureBySlackID returns the member ID for a Slack user, creating a row
// with no Forge user ID when none exists. It never overwrites the name or
// avatar of an existing row. Imports use it to credit Slack authors who
// have not signed in. now stamps created_at on a new row.
func EnsureBySlackID(ctx context.Context, tx *sql.Tx, slackUserID, name, avatarURL string, now time.Time) (int64, error) {
	if slackUserID == "" {
		return 0, errors.New("ensure member: empty Slack user ID")
	}
	if name == "" {
		name = slackUserID
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO members (slack_user_id, name, avatar_url, created_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (slack_user_id) DO NOTHING`,
		slackUserID, name, safeAvatar(avatarURL), now.Unix()); err != nil {
		return 0, fmt.Errorf("ensure member: %w", err)
	}
	var id int64
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM members WHERE slack_user_id = ?`, slackUserID).Scan(&id); err != nil {
		return 0, fmt.Errorf("ensure member: %w", err)
	}
	return id, nil
}
