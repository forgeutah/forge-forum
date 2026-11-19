package forum

import (
	"context"
	"database/sql"
	"fmt"
)

// Vote records (on) or withdraws (off) memberID's upvote on a thread or
// reply and returns the post's thread ID. Both directions are idempotent:
// a member has at most one vote per post, and the post's cached score
// changes only when a vote row does. Members may upvote their own posts.
// It returns ErrNotFound or ErrDeleted when the post cannot take votes.
func (s *Store) Vote(ctx context.Context, memberID int64, post PostRef, on bool) (int64, error) {
	var threadID int64
	err := s.db.WithWriteTx(ctx, func(tx *sql.Tx) error {
		table, err := post.Kind.table()
		if err != nil {
			return err
		}
		p, err := livePostTx(ctx, tx, post)
		if err != nil {
			return err
		}
		threadID = p.ThreadID

		query, delta := `DELETE FROM votes WHERE member_id = ? AND target_kind = ? AND target_id = ?`, -1
		args := []any{memberID, string(post.Kind), post.ID}
		if on {
			query, delta = `INSERT INTO votes (member_id, target_kind, target_id, created_at)
				VALUES (?, ?, ?, ?) ON CONFLICT DO NOTHING`, 1
			args = append(args, s.now().Unix())
		}
		res, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("vote: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE `+table+` SET score = score + ? WHERE id = ?`, delta, post.ID); err != nil {
			return fmt.Errorf("update score: %w", err)
		}
		return nil
	})
	return threadID, err
}
