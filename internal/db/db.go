// Package db opens forge-forum's SQLite database and applies migrations.
//
// The layout mirrors forge-proxy: one writer connection so writes never
// contend, a pool of read-only connections, WAL mode, and INTEGER unix
// second timestamps throughout.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"

	_ "modernc.org/sqlite"
)

// DB holds the writer and reader pools.
type DB struct {
	writer *sql.DB
	reader *sql.DB
}

// Open opens (creating if needed) the database at path and applies any
// pending migrations.
func Open(ctx context.Context, path string) (*DB, error) {
	writer, err := sql.Open("sqlite", dsn(path, false))
	if err != nil {
		return nil, fmt.Errorf("open writer: %w", err)
	}
	writer.SetMaxOpenConns(1)

	if err := migrate(ctx, writer); err != nil {
		writer.Close()
		return nil, err
	}

	reader, err := sql.Open("sqlite", dsn(path, true))
	if err != nil {
		writer.Close()
		return nil, fmt.Errorf("open reader: %w", err)
	}
	reader.SetMaxOpenConns(4)

	return &DB{writer: writer, reader: reader}, nil
}

func dsn(path string, readOnly bool) string {
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "foreign_keys(ON)")
	q.Add("_pragma", "busy_timeout(5000)")
	if readOnly {
		q.Add("mode", "ro")
	} else {
		// BEGIN IMMEDIATE takes the write lock up front, so a transaction
		// never fails halfway through on a lock upgrade.
		q.Add("_txlock", "immediate")
	}
	return "file:" + path + "?" + q.Encode()
}

// Close closes both pools.
func (d *DB) Close() error {
	rerr := d.reader.Close()
	if werr := d.writer.Close(); werr != nil {
		return werr
	}
	return rerr
}

// Reader returns the read-only pool for queries.
func (d *DB) Reader() *sql.DB { return d.reader }

// WithWriteTx runs fn inside a write transaction. The transaction commits
// when fn returns nil and rolls back otherwise.
func (d *DB) WithWriteTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := d.writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}
