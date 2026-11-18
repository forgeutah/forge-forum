package forum

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// ActiveTags returns the slugs members can pick, in the order admins
// curated them.
func (s *Store) ActiveTags(ctx context.Context) ([]string, error) {
	rows, err := s.db.Reader().QueryContext(ctx, `SELECT slug FROM tags WHERE active = 1 ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list tags: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return nil, fmt.Errorf("list tags: %w", err)
		}
		out = append(out, slug)
	}
	return out, rows.Err()
}

// resolveTagsTx maps slugs to tag IDs, dropping blanks and duplicates. It
// returns a ValidationError when none remain or one is not an active tag.
func resolveTagsTx(ctx context.Context, tx *sql.Tx, slugs []string) ([]int64, error) {
	var ids []int64
	seen := map[string]bool{}
	for _, slug := range slugs {
		slug = strings.TrimSpace(slug)
		if slug == "" || seen[slug] {
			continue
		}
		seen[slug] = true

		var id int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM tags WHERE slug = ? AND active = 1`, slug).Scan(&id)
		if err == sql.ErrNoRows {
			return nil, ValidationError{Field: "tags", Message: "One of those tags is not on the list. Pick from the tags shown."}
		}
		if err != nil {
			return nil, fmt.Errorf("look up tag: %w", err)
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, ValidationError{Field: "tags", Message: "Pick at least one tag."}
	}
	return ids, nil
}

// loadTags fills Tags on each thread with one query.
func (s *Store) loadTags(ctx context.Context, threads []Thread) error {
	if len(threads) == 0 {
		return nil
	}
	index := make(map[int64]int, len(threads))
	args := make([]any, len(threads))
	for i, t := range threads {
		index[t.ID] = i
		args[i] = t.ID
	}
	rows, err := s.db.Reader().QueryContext(ctx, `
		SELECT tt.thread_id, g.slug
		FROM thread_tags tt JOIN tags g ON g.id = tt.tag_id
		WHERE tt.thread_id IN (?`+strings.Repeat(", ?", len(args)-1)+`)
		ORDER BY g.id`, args...)
	if err != nil {
		return fmt.Errorf("load tags: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var threadID int64
		var slug string
		if err := rows.Scan(&threadID, &slug); err != nil {
			return fmt.Errorf("load tags: %w", err)
		}
		i := index[threadID]
		threads[i].Tags = append(threads[i].Tags, slug)
	}
	return rows.Err()
}
