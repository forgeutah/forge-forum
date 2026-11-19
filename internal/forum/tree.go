package forum

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"
)

// ReplyNode is one reply in a thread's tree, as one member sees it.
type ReplyNode struct {
	ID int64
	// ParentID is the reply this one answers, or 0 at the top level.
	ParentID int64

	AuthorID     int64
	AuthorName   string
	AuthorAvatar string

	BodyMD string
	Score  int
	// Voted reports whether the viewing member has upvoted the reply.
	Voted     bool
	CreatedAt time.Time
	EditedAt  *time.Time

	// Deleted marks a placeholder: the reply is gone but replies under it
	// remain. Its author, body, score, and edit mark are withheld. Removed
	// is also set when a moderator, not the author, took it down.
	Deleted bool
	Removed bool
	// Imported is set for replies moved from Slack.
	Imported bool

	// Depth is 0 at the top level.
	Depth    int
	Children []*ReplyNode
}

// ReplyCounts summarises a thread's replies.
type ReplyCounts struct {
	// Live counts replies that are not deleted.
	Live int
	// Imported counts replies that came from Slack with the thread.
	Imported int
}

// ReplyTree returns a thread's replies as viewerID sees them (plan KTD9):
// one query, nested in memory, siblings ordered by score then age. A
// deleted reply with a live reply somewhere under it is kept as a
// placeholder; one with none is left out.
func (s *Store) ReplyTree(ctx context.Context, threadID, viewerID int64) ([]*ReplyNode, ReplyCounts, error) {
	rows, err := s.db.Reader().QueryContext(ctx, `
		SELECT r.id, COALESCE(r.parent_id, 0), r.author_id, m.name, m.avatar_url,
			r.body_md, r.score, r.created_at, r.edited_at,
			r.deleted_at IS NOT NULL, r.removed_by IS NOT NULL, r.imported,
			EXISTS (SELECT 1 FROM votes v
			        WHERE v.member_id = ? AND v.target_kind = 'reply' AND v.target_id = r.id)
		FROM replies r JOIN members m ON m.id = r.author_id
		WHERE r.thread_id = ?
		ORDER BY r.id`, viewerID, threadID)
	if err != nil {
		return nil, ReplyCounts{}, fmt.Errorf("load replies: %w", err)
	}
	defer rows.Close()

	var all []*ReplyNode
	var counts ReplyCounts
	for rows.Next() {
		n := &ReplyNode{}
		var created int64
		var edited *int64
		if err := rows.Scan(&n.ID, &n.ParentID, &n.AuthorID, &n.AuthorName, &n.AuthorAvatar,
			&n.BodyMD, &n.Score, &created, &edited, &n.Deleted, &n.Removed, &n.Imported, &n.Voted); err != nil {
			return nil, ReplyCounts{}, fmt.Errorf("load replies: %w", err)
		}
		n.CreatedAt, n.EditedAt = time.Unix(created, 0), unixPtr(edited)
		if !n.Deleted {
			counts.Live++
		}
		if n.Imported {
			counts.Imported++
		}
		all = append(all, n)
	}
	if err := rows.Err(); err != nil {
		return nil, ReplyCounts{}, fmt.Errorf("load replies: %w", err)
	}
	return buildReplyTree(all), counts, nil
}

// buildReplyTree nests flat replies under their parents and returns the
// top level.
func buildReplyTree(all []*ReplyNode) []*ReplyNode {
	byID := make(map[int64]*ReplyNode, len(all))
	for _, n := range all {
		byID[n.ID] = n
	}
	var roots []*ReplyNode
	for _, n := range all {
		if parent, ok := byID[n.ParentID]; ok {
			parent.Children = append(parent.Children, n)
		} else {
			roots = append(roots, n)
		}
	}
	return arrangeReplies(roots, 0)
}

// arrangeReplies prunes, orders, and numbers one level of the tree and
// everything under it.
func arrangeReplies(level []*ReplyNode, depth int) []*ReplyNode {
	kept := level[:0]
	for _, n := range level {
		n.Depth = depth
		n.Children = arrangeReplies(n.Children, depth+1)
		// A deleted reply survives only as the stand-in for live replies
		// under it. Children are pruned first, so "has children" means
		// "has a live descendant".
		if n.Deleted && len(n.Children) == 0 {
			continue
		}
		kept = append(kept, n)
	}
	// Order before withholding scores: a placeholder keeps the position
	// its reply earned.
	slices.SortStableFunc(kept, func(a, b *ReplyNode) int {
		return cmp.Or(
			cmp.Compare(b.Score, a.Score),
			a.CreatedAt.Compare(b.CreatedAt),
			cmp.Compare(a.ID, b.ID),
		)
	})
	for _, n := range kept {
		if n.Deleted {
			*n = ReplyNode{
				ID: n.ID, ParentID: n.ParentID, CreatedAt: n.CreatedAt,
				Deleted: true, Removed: n.Removed, Depth: n.Depth, Children: n.Children,
			}
		}
	}
	return kept
}
