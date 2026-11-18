package db

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

// OpenTest opens a fresh database in a temp dir for a test.
func openTest(t *testing.T) (*DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "forum.db")
	d, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d, path
}

// TestOpen_FreshDatabase_AppliesMigrationsOnce checks that migrations run
// on first open and are not re-applied on the second.
func TestOpen_FreshDatabase_AppliesMigrationsOnce(t *testing.T) {
	ctx := context.Background()
	d, path := openTest(t)

	first, err := d.appliedMigrations(ctx)
	if err != nil {
		t.Fatalf("appliedMigrations: %v", err)
	}
	if len(first) == 0 {
		t.Fatal("no migrations applied to a fresh database")
	}

	var tags int
	if err := d.Reader().QueryRowContext(ctx, `SELECT COUNT(*) FROM tags`).Scan(&tags); err != nil {
		t.Fatalf("count tags: %v", err)
	}
	d.Close()

	d2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer d2.Close()
	second, _ := d2.appliedMigrations(ctx)
	if len(second) != len(first) {
		t.Errorf("migrations after reopen = %d, want %d", len(second), len(first))
	}
	var tags2 int
	d2.Reader().QueryRowContext(ctx, `SELECT COUNT(*) FROM tags`).Scan(&tags2)
	if tags2 != tags {
		t.Errorf("seed tags after reopen = %d, want %d (migration re-applied)", tags2, tags)
	}
}

// TestWithWriteTx_ErrorRollsBack checks that a failed transaction leaves
// no row behind.
func TestWithWriteTx_ErrorRollsBack(t *testing.T) {
	ctx := context.Background()
	d, _ := openTest(t)

	boom := errors.New("boom")
	err := d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO members (slack_user_id, name, created_at) VALUES ('U1', 'Ada', 1)`); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("WithWriteTx error = %v, want boom", err)
	}

	var n int
	d.Reader().QueryRowContext(ctx, `SELECT COUNT(*) FROM members`).Scan(&n)
	if n != 0 {
		t.Errorf("members after rollback = %d, want 0", n)
	}
}

// TestSchema_SlackSourceUnique checks the one-move-per-Slack-thread index.
func TestSchema_SlackSourceUnique(t *testing.T) {
	ctx := context.Background()
	d, _ := openTest(t)

	insert := func() error {
		return d.WithWriteTx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx,
				`INSERT OR IGNORE INTO members (id, slack_user_id, name, created_at) VALUES (1, 'U1', 'Ada', 1)`); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO threads
				(author_id, title, body_md, created_at, last_activity_at, slack_channel_id, slack_thread_ts)
				VALUES (1, 't', 'b', 1, 1, 'C1', '100.1')`)
			return err
		})
	}
	if err := insert(); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if err := insert(); err == nil {
		t.Error("second insert of the same Slack thread succeeded, want unique violation")
	}
}

// appliedMigrations returns the names of applied migrations.
func (d *DB) appliedMigrations(ctx context.Context) ([]string, error) {
	rows, err := d.reader.QueryContext(ctx, `SELECT name FROM schema_migrations ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
