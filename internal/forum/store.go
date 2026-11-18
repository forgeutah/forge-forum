// Package forum holds the forum's content: threads, tags, and (in later
// files) replies and votes.
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

// ErrNotFound is returned when a thread or reply does not exist.
var ErrNotFound = errors.New("forum: not found")

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
	db  *db.DB
	now func() time.Time
}

// NewStore returns a Store over database. now is the clock; pass time.Now
// outside tests.
func NewStore(database *db.DB, now func() time.Time) *Store {
	return &Store{db: database, now: now}
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
