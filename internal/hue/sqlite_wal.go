package hue

// Opening a SQLite file that several worker processes share.
//
// DuckDB runs several worker processes at once, and each opens the same
// store. Switching a fresh file to WAL takes an exclusive lock, so the
// processes race: the losers see SQLITE_BUSY/SQLITE_LOCKED, and on Windows
// often a transient I/O error (e.g. SQLITE_IOERR_TRUNCATE, 1546, while
// another process maps the -shm file) instead of waiting. This is the
// equivalent of vgi-python's vgi/_sqlite_wal.py:
//
//   - busy_timeout is set on every connection before anything else (DSN),
//   - the WAL switch runs once, retried on BUSY/LOCKED/IOERR with bounded
//     backoff (5 ms doubling to 100 ms, ~30 s in all), never per connection,
//   - schema setup runs inside BEGIN IMMEDIATE, so one process creates it and
//     the others wait for it.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"modernc.org/sqlite"
)

const (
	walBusyTimeoutMS = 30000
	walRetryFirst    = 5 * time.Millisecond
	walRetryMax      = 100 * time.Millisecond
	walRetryTotal    = 30 * time.Second
)

// SQLite primary result codes (the low byte of an extended code).
const (
	sqliteBusy   = 5
	sqliteLocked = 6
	sqliteIOErr  = 10
)

// walRetryable reports whether err is a lock conflict or a transient I/O
// error that another process's concurrent open can cause.
func walRetryable(err error) bool {
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return false
	}
	switch se.Code() & 0xff {
	case sqliteBusy, sqliteLocked, sqliteIOErr:
		return true
	}
	return false
}

// withWALRetry runs fn until it succeeds, fails with a non-retryable error,
// or walRetryTotal has passed.
func withWALRetry(ctx context.Context, fn func() error) error {
	deadline := time.Now().Add(walRetryTotal)
	delay := walRetryFirst
	for {
		err := fn()
		if err == nil || !walRetryable(err) || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(delay):
		}
		delay = min(delay*2, walRetryMax)
	}
}

// openSharedSQLite opens the SQLite file at path in WAL mode and runs schema
// (CREATE ... IF NOT EXISTS statements) inside BEGIN IMMEDIATE. Safe to call
// from many processes at once on a fresh file.
func openSharedSQLite(path, schema string) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	// One absolute path for every process, so all of them share one -wal and
	// -shm next to it. busy_timeout is applied first on every new connection.
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(abs)+fmt.Sprintf("?_pragma=busy_timeout(%d)", walBusyTimeoutMS))
	if err != nil {
		return nil, err
	}
	if err := initSharedSQLite(context.Background(), db, schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("opening %s: %w", abs, err)
	}
	return db, nil
}

func initSharedSQLite(ctx context.Context, db *sql.DB, schema string) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if err := withWALRetry(ctx, func() error {
		var mode string
		if err := conn.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&mode); err != nil {
			return fmt.Errorf("applying \"PRAGMA journal_mode=WAL\": %w", err)
		}
		return nil
	}); err != nil {
		return err
	}
	return withWALRetry(ctx, func() error {
		if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, schema); err != nil {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
			return err
		}
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
			return err
		}
		return nil
	})
}
