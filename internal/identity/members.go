// Package identity turns forge-proxy's upstream headers into forum members.
//
// Members are keyed on Slack user ID (plan KTD3). A row may exist before
// its owner ever signs in, because moving a Slack thread credits its Slack
// authors; the Forge user ID is attached when that person first arrives.
package identity

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Member is a forum member as seen by one request.
type Member struct {
	ID          int64
	SlackUserID string
	Name        string
	AvatarURL   string
	DMReplies   bool
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
		RETURNING id, dm_replies`,
		m.SlackUserID, forgeUserID, m.Name, m.AvatarURL, now.Unix(), now.Unix(),
	).Scan(&m.ID, &m.DMReplies)
	if err != nil {
		return Member{}, fmt.Errorf("upsert member: %w", err)
	}
	return m, nil
}

// EnsureBySlackID returns the member ID for a Slack user, creating a row
// with no Forge user ID when none exists. It never overwrites the name or
// avatar of an existing row. Imports use it to credit Slack authors who
// have not signed in.
func EnsureBySlackID(ctx context.Context, tx *sql.Tx, slackUserID, name, avatarURL string) (int64, error) {
	if slackUserID == "" {
		return 0, fmt.Errorf("ensure member: empty Slack user ID")
	}
	if name == "" {
		name = slackUserID
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO members (slack_user_id, name, avatar_url, created_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (slack_user_id) DO NOTHING`,
		slackUserID, name, safeAvatar(avatarURL), time.Now().Unix()); err != nil {
		return 0, fmt.Errorf("ensure member: %w", err)
	}
	var id int64
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM members WHERE slack_user_id = ?`, slackUserID).Scan(&id); err != nil {
		return 0, fmt.Errorf("ensure member: %w", err)
	}
	return id, nil
}
