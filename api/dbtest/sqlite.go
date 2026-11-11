package dbtest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"time"

	"modernc.org/sqlite"
)

func openSqliteDatabase(action string, settings config) (database, error) {
	if settings.databasePath == "" {
		return database{}, errors.New("set SQLITE_DATABASE_PATH or --database to a persistent file")
	}
	path, err := filepath.Abs(settings.databasePath)
	if err != nil {
		return database{}, errors.New("invalid SQLite database path")
	}
	mode := "rw"
	if action == "check" || action == "serve" {
		mode = "ro"
	} else if (action == "seed" || action == "serve") && !settings.requireExisting {
		mode = "rwc"
	}
	parameters := url.Values{"mode": {mode}, "_pragma": {"busy_timeout(5000)"}}
	if action == "seed" || action == "advance" {
		parameters.Set("_txlock", "immediate")
		parameters.Add("_pragma", "synchronous(FULL)")
	}
	address := (&url.URL{Scheme: "file", Path: path, RawQuery: parameters.Encode()}).String()
	connection, err := sql.Open("sqlite", address)
	if err != nil {
		return database{}, fmt.Errorf("configure SQLite connection: %w", err)
	}
	connection.SetMaxOpenConns(1)
	return database{
		connection:   connection,
		driver:       "sqlite",
		binaryType:   "BLOB",
		tablesQuery:  "SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name IN ('borq_probe_state', 'borq_probe_records')",
		versionQuery: "SELECT sqlite_version()",
	}, nil
}

func enableWal(ctx context.Context, connection *sql.DB) error {
	for {
		var mode string
		err := connection.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&mode)
		if err == nil {
			if mode != "wal" {
				return fmt.Errorf("expected SQLite WAL mode, got %q", mode)
			}
			return nil
		}
		// Switching journal mode can return BUSY without honoring busy_timeout.
		var sqliteError *sqlite.Error
		if !errors.As(err, &sqliteError) || sqliteError.Code()&0xff != 5 {
			return fmt.Errorf("enable SQLite WAL: %w", err)
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
