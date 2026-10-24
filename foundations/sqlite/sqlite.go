package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	"github.com/gofrs/flock"
	_ "modernc.org/sqlite"
)

// Stable error identities let callers distinguish missing records and concurrent writes.
var (
	ErrNotFound = errors.New("record not found")
	ErrConflict = errors.New("record revision changed")
)

// Database owns the shared SQL connection pool; database/sql requires pointer ownership.
type Database struct {
	connection *sql.DB
	// The lock library requires shared pointer ownership of the process-wide file lock.
	lock *flock.Flock
}

// Record holds a JSON document and its monotonically increasing storage revision.
type Record struct {
	Collection string
	Key        string
	Value      json.RawMessage
	Revision   int64
}

// Write compares the previous revision before replacing a document. Revision zero creates it.
type Write struct {
	Collection       string
	Key              string
	Value            json.RawMessage
	ExpectedRevision int64
}

// New opens a private persistent database. WAL files must remain on the same host as the server.
func New(ctx context.Context, path string) (Database, error) {
	if path == "" {
		return Database{}, errors.New("sqlite database path is required")
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return Database{}, fmt.Errorf("resolve database path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(absolutePath), 0o700); err != nil {
		return Database{}, fmt.Errorf("create database directory: %w", err)
	}
	lock := flock.New(absolutePath + ".lock")
	locked, err := lock.TryLock()
	if err != nil {
		return Database{}, fmt.Errorf("lock database: %w", err)
	}
	if !locked {
		_ = lock.Close()
		return Database{}, errors.New("database is already open by another Borq server")
	}
	// SQLite derives WAL and shared-memory permissions from the database's initial mode.
	file, err := os.OpenFile(absolutePath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		_ = lock.Close()
		return Database{}, fmt.Errorf("create database file: %w", err)
	}
	permissionError := file.Chmod(0o600)
	closeError := file.Close()
	if err := errors.Join(permissionError, closeError); err != nil {
		_ = lock.Close()
		return Database{}, fmt.Errorf("protect database file: %w", err)
	}
	location := url.URL{Scheme: "file", Path: absolutePath}
	parameters := location.Query()
	for _, pragma := range []string{"busy_timeout(5000)", "journal_mode(WAL)", "synchronous(FULL)"} {
		parameters.Add("_pragma", pragma)
	}
	location.RawQuery = parameters.Encode()
	connection, err := sql.Open("sqlite", location.String())
	if err != nil {
		_ = lock.Close()
		return Database{}, fmt.Errorf("open sqlite: %w", err)
	}
	connection.SetMaxOpenConns(1)
	database := Database{connection: connection, lock: lock}
	if err := database.initialize(ctx); err != nil {
		_ = connection.Close()
		_ = lock.Close()
		return Database{}, err
	}
	if err := os.Chmod(absolutePath, 0o600); err != nil {
		_ = connection.Close()
		_ = lock.Close()
		return Database{}, fmt.Errorf("protect database file: %w", err)
	}
	return database, nil
}

func (database Database) initialize(ctx context.Context) error {
	var version int
	if err := database.connection.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read database version: %w", err)
	}
	if version > 1 {
		return fmt.Errorf("database version %d is newer than this server supports", version)
	}
	if version == 1 {
		return nil
	}
	transaction, err := database.connection.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin database migration: %w", err)
	}
	defer transaction.Rollback()
	_, err = transaction.ExecContext(ctx, `CREATE TABLE records (
		collection TEXT NOT NULL,
		key TEXT NOT NULL,
		value TEXT NOT NULL CHECK(json_valid(value)),
		revision INTEGER NOT NULL CHECK(revision > 0),
		PRIMARY KEY(collection, key)
	) STRICT`)
	if err != nil {
		return fmt.Errorf("create document table: %w", err)
	}
	if _, err := transaction.ExecContext(ctx, "PRAGMA user_version = 1"); err != nil {
		return fmt.Errorf("record database version: %w", err)
	}
	return transaction.Commit()
}

// Close releases the database after application workers have stopped.
func (database Database) Close() error {
	return errors.Join(database.connection.Close(), database.lock.Close())
}

// Ping verifies that persistent state remains accessible to the running server.
func (database Database) Ping(ctx context.Context) error {
	return database.connection.PingContext(ctx)
}

// Get reads the document and revision together for subsequent compare-and-swap writes.
func (database Database) Get(ctx context.Context, collection, key string) (Record, error) {
	record := Record{Collection: collection, Key: key}
	var value string
	err := database.connection.QueryRowContext(ctx,
		"SELECT value, revision FROM records WHERE collection = ? AND key = ?", collection, key,
	).Scan(&value, &record.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, fmt.Errorf("read document: %w", err)
	}
	record.Value = json.RawMessage(value)
	return record, nil
}

// List reads a deterministic snapshot of one collection, including unfinished operations.
func (database Database) List(ctx context.Context, collection string) ([]Record, error) {
	rows, err := database.connection.QueryContext(ctx,
		"SELECT key, value, revision FROM records WHERE collection = ? ORDER BY key", collection)
	if err != nil {
		return nil, fmt.Errorf("list documents: %w", err)
	}
	defer rows.Close()
	records := []Record{}
	for rows.Next() {
		record := Record{Collection: collection}
		var value string
		if err := rows.Scan(&record.Key, &value, &record.Revision); err != nil {
			return nil, fmt.Errorf("read document row: %w", err)
		}
		record.Value = json.RawMessage(value)
		records = append(records, record)
	}
	return records, rows.Err()
}

// Commit atomically checks and writes all supplied documents, without holding locks across network calls.
func (database Database) Commit(ctx context.Context, writes []Write) error {
	for _, write := range writes {
		if write.Collection == "" || write.Key == "" || write.ExpectedRevision < 0 || !json.Valid(write.Value) {
			return errors.New("invalid sqlite document write")
		}
	}
	transaction, err := database.connection.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin document transaction: %w", err)
	}
	defer transaction.Rollback()
	for _, write := range writes {
		var result sql.Result
		if write.ExpectedRevision == 0 {
			result, err = transaction.ExecContext(ctx,
				"INSERT INTO records(collection, key, value, revision) VALUES (?, ?, ?, 1) ON CONFLICT DO NOTHING",
				write.Collection, write.Key, string(write.Value))
		} else {
			result, err = transaction.ExecContext(ctx,
				"UPDATE records SET value = ?, revision = revision + 1 WHERE collection = ? AND key = ? AND revision = ?",
				string(write.Value), write.Collection, write.Key, write.ExpectedRevision)
		}
		if err != nil {
			return fmt.Errorf("write document: %w", err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("check document write: %w", err)
		}
		if count != 1 {
			return ErrConflict
		}
	}
	return transaction.Commit()
}
