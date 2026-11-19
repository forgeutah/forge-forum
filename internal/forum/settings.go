package forum

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// DMReplies reports whether a member wants a Slack message when someone
// replies to them (plan R13).
func (s *Store) DMReplies(ctx context.Context, memberID int64) (bool, error) {
	var on bool
	err := s.db.Reader().QueryRowContext(ctx, `SELECT dm_replies FROM members WHERE id = ?`, memberID).Scan(&on)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, fmt.Errorf("get member setting: %w", err)
	}
	return on, nil
}

// SetDMReplies turns a member's reply messages on or off.
func (s *Store) SetDMReplies(ctx context.Context, memberID int64, on bool) error {
	return s.db.WithWriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE members SET dm_replies = ? WHERE id = ?`, on, memberID); err != nil {
			return fmt.Errorf("set member setting: %w", err)
		}
		return nil
	})
}
