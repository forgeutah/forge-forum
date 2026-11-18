package notify

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/forgeutah/forge-forum/internal/db"
	"github.com/forgeutah/forge-forum/internal/slackx"
)

const (
	// batchSize is how many due rows one query loads.
	batchSize = 20
	// maxAttempts is how many sends a row gets before it is marked failed.
	maxAttempts = 5
	// defaultInterval is how often Run looks for due rows when nothing
	// wakes it.
	defaultInterval = 5 * time.Second
	// maxErrorLen caps what is stored in last_error.
	maxErrorLen = 500
)

// backoff[n-1] is the wait after the nth failed attempt.
var backoff = [maxAttempts - 1]time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute, 30 * time.Minute}

// permanentCodes are Slack errors that retrying cannot fix: the target
// does not exist or cannot be written to, or the token is no good.
var permanentCodes = map[string]bool{
	slackx.ErrChannelNotFound: true,
	slackx.ErrNotInChannel:    true,
	"is_archived":             true,
	slackx.ErrUserNotFound:    true,
	"cannot_dm_bot":           true,
	"msg_too_long":            true,
	"no_text":                 true,
	"account_inactive":        true,
	"invalid_auth":            true,
	"not_authed":              true,
	"token_revoked":           true,
	"missing_scope":           true,
}

// Row statuses, matching the notifications.status CHECK constraint.
const (
	statusPending = "pending"
	statusSent    = "sent"
	statusFailed  = "failed"
	statusSkipped = "skipped"
)

// Worker delivers pending outbox rows.
type Worker struct {
	db     *db.DB
	client slackx.Client
	logger *slog.Logger
	now    func() time.Time

	interval time.Duration
	wake     chan struct{}
	// running keeps passes from overlapping, so a row is never sent by
	// two passes at once.
	running sync.Mutex
}

// NewWorker returns a worker. client nil means Slack is not configured:
// every pending row is marked 'skipped'.
func NewWorker(database *db.DB, client slackx.Client, logger *slog.Logger, now func() time.Time) *Worker {
	return &Worker{
		db:       database,
		client:   client,
		logger:   logger,
		now:      now,
		interval: defaultInterval,
		wake:     make(chan struct{}, 1),
	}
}

// Run delivers due rows every few seconds, and at once after Wake, until
// ctx is done.
func (w *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		if _, err := w.RunOnce(ctx); err != nil && ctx.Err() == nil {
			w.logger.Error("notification pass failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-w.wake:
		}
	}
}

// Wake asks Run to deliver now instead of waiting for the next interval.
// It never blocks.
func (w *Worker) Wake() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// outboxRow is one pending notification.
type outboxRow struct {
	id       int64
	kind     string
	payload  string
	attempts int
}

// RunOnce delivers every row that is due and returns how many reached a
// terminal state (sent, failed, or skipped).
func (w *Worker) RunOnce(ctx context.Context) (int, error) {
	w.running.Lock()
	defer w.running.Unlock()

	// One reading of the clock per pass, and an id cursor: a row that is
	// rescheduled during the pass is not picked up again by it.
	now := w.now()
	done := 0
	var afterID int64
	for {
		rows, err := w.due(ctx, now, afterID)
		if err != nil {
			return done, err
		}
		if len(rows) == 0 {
			return done, nil
		}
		for _, r := range rows {
			afterID = r.id
			terminal, err := w.deliver(ctx, r, now)
			if err != nil {
				return done, err
			}
			if terminal {
				done++
			}
		}
	}
}

// due loads the next batch of rows to handle, oldest first. Without a
// client nothing will ever be sent, so every pending row qualifies.
func (w *Worker) due(ctx context.Context, now time.Time, afterID int64) ([]outboxRow, error) {
	query := `SELECT id, kind, payload, attempts FROM notifications
		WHERE status = 'pending' AND id > ? AND next_attempt_at <= ?
		ORDER BY id LIMIT ?`
	args := []any{afterID, now.Unix(), batchSize}
	if w.client == nil {
		query = `SELECT id, kind, payload, attempts FROM notifications
			WHERE status = 'pending' AND id > ?
			ORDER BY id LIMIT ?`
		args = []any{afterID, batchSize}
	}
	rows, err := w.db.Reader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("notify: load due rows: %w", err)
	}
	defer rows.Close()

	var out []outboxRow
	for rows.Next() {
		var r outboxRow
		if err := rows.Scan(&r.id, &r.kind, &r.payload, &r.attempts); err != nil {
			return nil, fmt.Errorf("notify: scan due row: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("notify: load due rows: %w", err)
	}
	return out, nil
}

// deliver handles one row and reports whether it reached a terminal
// state. The returned error is for failures of the worker itself (the
// database, or ctx ending), never for Slack refusing a message.
func (w *Worker) deliver(ctx context.Context, r outboxRow, now time.Time) (bool, error) {
	if w.client == nil {
		return true, w.finish(ctx, r, statusSkipped, r.attempts, "", now)
	}

	var msg Message
	if err := json.Unmarshal([]byte(r.payload), &msg); err != nil {
		return true, w.finish(ctx, r, statusFailed, r.attempts, "undecodable payload: "+err.Error(), now)
	}
	if msg.Channel == "" {
		if Kind(r.kind) == KindAnnounce {
			// No announcement channel is configured.
			return true, w.finish(ctx, r, statusSkipped, r.attempts, "", now)
		}
		return true, w.finish(ctx, r, statusFailed, r.attempts, "payload has no channel", now)
	}
	if msg.Text == "" {
		return true, w.finish(ctx, r, statusFailed, r.attempts, "payload has no text", now)
	}

	// The send happens outside any write transaction so Slack's latency
	// never holds the database's single writer. If the process dies after
	// Slack accepts the message and before the row is marked sent, the
	// message is sent again on restart; a rare duplicate is accepted over
	// a lost notification.
	err := w.client.PostMessage(ctx, msg.Channel, msg.Text, msg.ThreadTS)
	attempts := r.attempts + 1
	if err == nil {
		return true, w.finish(ctx, r, statusSent, attempts, "", now)
	}
	if ctx.Err() != nil {
		// Shutting down is not Slack's failure; leave the row as it was.
		return false, ctx.Err()
	}
	if permanentCodes[slackx.ErrorCode(err)] || attempts >= maxAttempts {
		return true, w.finish(ctx, r, statusFailed, attempts, err.Error(), now)
	}

	wait := backoff[attempts-1]
	var limited *slackx.RateLimitedError
	if errors.As(err, &limited) && limited.RetryAfter > wait {
		wait = limited.RetryAfter
	}
	return false, w.reschedule(ctx, r, attempts, err.Error(), now.Add(wait))
}

// finish moves a row to a terminal status. The write does not stop when
// ctx is cancelled: a message Slack has accepted must not stay pending,
// and be sent again, because shutdown began before the row was marked.
func (w *Worker) finish(ctx context.Context, r outboxRow, status string, attempts int, lastError string, now time.Time) error {
	ctx = context.WithoutCancel(ctx)
	var sentAt any
	if status == statusSent {
		sentAt = now.Unix()
	}
	lastError = capError(lastError)
	err := w.db.WithWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE notifications SET status = ?, attempts = ?, last_error = ?, sent_at = ?
			 WHERE id = ? AND status = 'pending'`,
			status, attempts, lastError, sentAt, r.id)
		return err
	})
	if err != nil {
		return fmt.Errorf("notify: mark row %d %s: %w", r.id, status, err)
	}

	attrs := []any{"id", r.id, "kind", r.kind, "status", status, "attempts", attempts}
	if status == statusFailed {
		w.logger.Warn("notification failed", append(attrs, "error", lastError)...)
	} else {
		w.logger.Info("notification "+status, attrs...)
	}
	return nil
}

// reschedule records a failed attempt and when to try again. Like finish,
// it writes even when ctx is cancelled, so the attempt is not lost.
func (w *Worker) reschedule(ctx context.Context, r outboxRow, attempts int, lastError string, next time.Time) error {
	ctx = context.WithoutCancel(ctx)
	lastError = capError(lastError)
	err := w.db.WithWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE notifications SET attempts = ?, last_error = ?, next_attempt_at = ?
			 WHERE id = ? AND status = 'pending'`,
			attempts, lastError, next.Unix(), r.id)
		return err
	})
	if err != nil {
		return fmt.Errorf("notify: reschedule row %d: %w", r.id, err)
	}
	w.logger.Info("notification will retry",
		"id", r.id, "kind", r.kind, "status", statusPending, "attempts", attempts,
		"next_attempt_at", next.Unix(), "error", lastError)
	return nil
}

func capError(s string) string {
	if r := []rune(s); len(r) > maxErrorLen {
		return string(r[:maxErrorLen])
	}
	return s
}
