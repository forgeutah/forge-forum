// Package forum holds the forum's content: threads, tags, replies, and
// votes, and decides who may change them.
//
// Reads go to the database's reader pool. Every write runs inside
// db.WithWriteTx, and the work is done by helpers that take the *sql.Tx so
// that callers which already hold a transaction (the Slack importer, the
// notification outbox) can compose them.
package forum

import (
	"errors"
	"time"

	"github.com/forgeutah/forge-forum/internal/db"
)

// ErrNotFound is returned when a thread, reply, tag, or member does not
// exist.
var ErrNotFound = errors.New("forum: not found")

// ErrDeleted is returned when an action targets a post that was deleted
// or removed, or one inside a deleted thread.
var ErrDeleted = errors.New("forum: post was deleted")

// ErrForbidden is returned when the acting member may not do what they
// asked: changing someone else's post, or an admin action without the
// admin role.
var ErrForbidden = errors.New("forum: not allowed")

// ValidationError reports input a member can fix. Message is written for
// the member and is safe to show on the form; Field names the input
// ("title", "body", "tags").
type ValidationError struct {
	Field   string
	Message string
}

func (e ValidationError) Error() string { return e.Message }

// Store reads and writes forum content.
type Store struct {
	db       *db.DB
	now      func() time.Time
	notifier Notifier
	// replyListener hears every reply posted on the forum, after its
	// transaction commits. It may be nil.
	replyListener func(threadID, replyID, parentID, authorID int64)
}

// NewStore returns a Store over database. now is the clock; pass time.Now
// outside tests.
func NewStore(database *db.DB, now func() time.Time) *Store {
	return &Store{db: database, now: now}
}

// SetReplyListener registers fn to be called after each reply posted on
// the forum has committed: never for a reply that was refused or rolled
// back, and never for replies imported from Slack. parentID is 0 for a
// reply to the thread itself. fn runs on the posting request's goroutine,
// so it must not block. Pass nil to remove the listener. Call it before
// the store is shared between goroutines.
func (s *Store) SetReplyListener(fn func(threadID, replyID, parentID, authorID int64)) {
	s.replyListener = fn
}

// Now returns the store's current time, so callers that format relative
// times agree with the timestamps the store writes.
func (s *Store) Now() time.Time { return s.now() }

// unixPtr converts a nullable unix-seconds column to a *time.Time.
func unixPtr(v *int64) *time.Time {
	if v == nil {
		return nil
	}
	t := time.Unix(*v, 0)
	return &t
}
