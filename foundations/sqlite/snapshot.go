package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
)

// Snapshot uses SQLite's native consistent VACUUM INTO operation without copying live WAL files.
// Destination must not exist and remains private to the caller until validation succeeds.
func (database Database) Snapshot(ctx context.Context, destination string) error {
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return errors.New("SQLite snapshot destination must be a fresh private file")
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(destination)
		return errors.New("prepare private SQLite snapshot")
	}
	if _, err := database.connection.ExecContext(ctx, "VACUUM INTO ?", destination); err != nil {
		_ = os.Remove(destination)
		return errors.New("create native SQLite snapshot")
	}
	if err := os.Chmod(destination, 0600); err != nil {
		_ = os.Remove(destination)
		return errors.New("protect SQLite snapshot")
	}
	if err := ValidateSnapshot(ctx, destination); err != nil {
		_ = os.Remove(destination)
		return err
	}
	return nil
}

// ValidateSnapshot checks physical integrity and the supported Borq schema without modifying the snapshot.
func ValidateSnapshot(ctx context.Context, file string) error {
	location := url.URL{Scheme: "file", Path: file}
	query := location.Query()
	query.Set("mode", "ro")
	location.RawQuery = query.Encode()
	connection, err := sql.Open("sqlite", location.String())
	if err != nil {
		return errors.New("open SQLite snapshot")
	}
	defer connection.Close()
	var integrity string
	if connection.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity) != nil || integrity != "ok" {
		return errors.New("SQLite snapshot integrity check failed")
	}
	var version int
	if connection.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version) != nil || version != 1 {
		return errors.New("SQLite snapshot schema is unsupported")
	}
	var count int
	if connection.QueryRowContext(ctx, "SELECT count(*) FROM records").Scan(&count) != nil {
		return errors.New("SQLite snapshot document table is invalid")
	}
	return nil
}

// ListBounded reads one collection only when its document count and encoded bytes fit the recovery budget.
// The read transaction keeps the preflight and body reads on the same SQLite snapshot.
func (database Database) ListBounded(ctx context.Context, collection string, maximumCount int, maximumBytes int64) ([]Record, error) {
	if maximumCount <= 0 || maximumBytes <= 0 {
		return nil, errors.New("SQLite collection limits must be positive")
	}
	transaction, err := database.connection.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, errors.New("open bounded SQLite read")
	}
	defer transaction.Rollback()
	var count int
	var bytes int64
	if transaction.QueryRowContext(ctx, "SELECT count(*), coalesce(sum(length(CAST(key AS BLOB)) + length(CAST(value AS BLOB))), 0) FROM records WHERE collection = ?", collection).Scan(&count, &bytes) != nil {
		return nil, errors.New("inspect SQLite collection bounds")
	}
	if count > maximumCount || bytes > maximumBytes {
		return nil, errors.New("SQLite collection exceeds native recovery count or byte limits")
	}
	rows, err := transaction.QueryContext(ctx, "SELECT key, value, revision FROM records WHERE collection = ? ORDER BY key", collection)
	if err != nil {
		return nil, errors.New("read bounded SQLite collection")
	}
	defer rows.Close()
	records := make([]Record, 0, count)
	for rows.Next() {
		record := Record{Collection: collection}
		var value string
		if rows.Scan(&record.Key, &value, &record.Revision) != nil {
			return nil, errors.New("read bounded SQLite document")
		}
		record.Value = []byte(value)
		records = append(records, record)
	}
	return records, rows.Err()
}
