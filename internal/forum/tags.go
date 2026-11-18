package forum

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
)

// Tag is one entry on the curated list. A retired tag (Active false)
// stays on the threads that carry it but cannot be picked or filtered by.
type Tag struct {
	Slug   string
	Active bool
}

// tagSlugPattern is what a tag may be called: lowercase letters, digits,
// and hyphens, 2 to 32 characters.
var tagSlugPattern = regexp.MustCompile(`^[a-z0-9-]{2,32}$`)

// AllTags returns every tag, retired ones included, in curated order.
func (s *Store) AllTags(ctx context.Context) ([]Tag, error) {
	rows, err := s.db.Reader().QueryContext(ctx, `SELECT slug, active FROM tags ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list tags: %w", err)
	}
	defer rows.Close()
	var out []Tag
	for rows.Next() {
		var t Tag
		if err := rows.Scan(&t.Slug, &t.Active); err != nil {
			return nil, fmt.Errorf("list tags: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// AddTag puts a new tag on the list. Only an admin may. The slug is
// lowercased and a leading "#" dropped; it returns a ValidationError when
// the result is malformed or already on the list.
func (s *Store) AddTag(ctx context.Context, actor Actor, slug string) error {
	if !actor.IsAdmin {
		return ErrForbidden
	}
	slug = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(slug), "#"))
	if !tagSlugPattern.MatchString(slug) {
		return ValidationError{Field: "slug", Message: "Use 2 to 32 lowercase letters, digits, or hyphens."}
	}
	return s.db.WithWriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO tags (slug, created_at) VALUES (?, ?) ON CONFLICT (slug) DO NOTHING`, slug, s.now().Unix())
		if err != nil {
			return fmt.Errorf("add tag: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ValidationError{Field: "slug", Message: "That tag is already on the list."}
		}
		return nil
	})
}

// SetTagActive retires a tag or brings it back. Only an admin may. It
// returns ErrNotFound for a slug that is not on the list.
func (s *Store) SetTagActive(ctx context.Context, actor Actor, slug string, active bool) error {
	if !actor.IsAdmin {
		return ErrForbidden
	}
	return s.db.WithWriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE tags SET active = ? WHERE slug = ?`, active, slug)
		if err != nil {
			return fmt.Errorf("set tag active: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

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
// When an existing thread is being edited, keepFor is its ID and the tags
// it already carries are accepted even if since retired; pass 0 otherwise.
func resolveTagsTx(ctx context.Context, tx *sql.Tx, slugs []string, keepFor int64) ([]int64, error) {
	var ids []int64
	seen := map[string]bool{}
	for _, slug := range slugs {
		slug = strings.TrimSpace(slug)
		if slug == "" || seen[slug] {
			continue
		}
		seen[slug] = true

		var id int64
		err := tx.QueryRowContext(ctx, `
			SELECT id FROM tags
			WHERE slug = ? AND (active = 1
				OR id IN (SELECT tag_id FROM thread_tags WHERE thread_id = ?))`, slug, keepFor).Scan(&id)
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
