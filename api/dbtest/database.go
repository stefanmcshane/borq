package dbtest

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// seedScript is embedded so the workload image needs only its binary and database storage.
//
//go:embed seed.sql
var seedScript string

// database/sql owns connection and transaction state through pointers throughout this package.
type database struct {
	connection   *sql.DB
	driver       string
	binaryType   string
	tablesQuery  string
	versionQuery string
	writeLock    string
	isolation    sql.IsolationLevel
}

type report struct {
	// Driver identifies the selected database implementation.
	Driver string `json:"driver"`
	// DatabaseVersion records the engine actually used for this check.
	DatabaseVersion string `json:"database_version"`
	// FixtureVersion identifies the fixed test-data format, independently of engine versions.
	FixtureVersion int `json:"fixture_version"`
	// Dataset identifies the expected CI run or recovery scenario.
	Dataset string `json:"dataset"`
	// Checkpoint is the exact externally expected generation of random values.
	Checkpoint int64 `json:"checkpoint"`
	// Samples exposes three verified random numbers for human comparison between versions.
	Samples []int64 `json:"samples"`
	// Rows is the verified number of seed records.
	Rows int `json:"rows"`
	// Checksum fingerprints the actual row values for comparison across recovery targets.
	Checksum string `json:"checksum"`
}

type record struct {
	id       int
	label    string
	amount   int64
	optional sql.NullString
	payload  []byte
}

func openDatabase(action string, settings config) (database, error) {
	switch settings.driver {
	case "sqlite":
		return openSqliteDatabase(action, settings)
	case "postgres":
		return openPostgresDatabase(settings)
	default:
		return database{}, errors.New("unsupported database driver")
	}
}

func beginWrite(ctx context.Context, store database) (*sql.Tx, error) {
	transaction, err := store.connection.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin fixture transaction: %w", err)
	}
	if store.writeLock != "" {
		// Concurrent copies must agree on first-time initialization of this fixture.
		if _, err := transaction.ExecContext(ctx, store.writeLock); err != nil {
			_ = transaction.Rollback()
			return nil, fmt.Errorf("lock fixture: %w", err)
		}
	}
	return transaction, nil
}

func initialize(ctx context.Context, store database, settings config) error {
	if store.driver == "sqlite" {
		if err := enableWal(ctx, store.connection); err != nil {
			return err
		}
	}
	transaction, err := beginWrite(ctx, store)
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	var tables int
	if err := transaction.QueryRowContext(ctx, store.tablesQuery).Scan(&tables); err != nil {
		return fmt.Errorf("inspect fixture tables: %w", err)
	}
	if tables == 2 {
		return nil
	}
	if tables != 0 {
		return errors.New("fixture is partially missing; refusing to reseed")
	}
	if settings.requireExisting {
		return errors.New("fixture is missing and require-existing forbids seeding")
	}
	if _, err := transaction.ExecContext(ctx, strings.ReplaceAll(seedScript, "{{binary}}", store.binaryType)); err != nil {
		return fmt.Errorf("create fixture: %w", err)
	}
	if _, err := transaction.ExecContext(ctx, "INSERT INTO borq_probe_state (id, version, dataset, checkpoint) VALUES (1, 2, $1, $2)", settings.expected.Dataset, settings.expected.Checkpoint); err != nil {
		return fmt.Errorf("record dataset identity: %w", err)
	}
	insert, err := transaction.PrepareContext(ctx, "INSERT INTO borq_probe_records (id, label, amount, optional_text, payload) VALUES ($1, $2, $3, $4, $5)")
	if err != nil {
		return fmt.Errorf("prepare seed: %w", err)
	}
	defer insert.Close()
	for id := 1; id <= 128; id++ {
		expected := expectedRecord(settings.expected, id)
		if _, err := insert.ExecContext(ctx, expected.id, expected.label, expected.amount, expected.optional, expected.payload); err != nil {
			return fmt.Errorf("seed row %d: %w", id, err)
		}
	}
	return transaction.Commit()
}

func expectedRecord(expected manifest, id int) record {
	// A CI-generated random seed makes values fresh while allowing offline reproduction.
	fingerprint := hmac.New(sha256.New, []byte(expected.Seed))
	_, _ = fmt.Fprintf(fingerprint, "%s/%d/%d", expected.Dataset, expected.Checkpoint, id)
	value := fingerprint.Sum(nil)
	amount := int64(binary.BigEndian.Uint64(value[:8]) >> 1)
	if id%2 != 0 {
		amount = -amount
	}
	return record{
		id:       id,
		label:    fmt.Sprintf("%s:checkpoint-%d:row-%03d: café 日本語 'quoted'\nsecond line", expected.Dataset, expected.Checkpoint, id),
		amount:   amount,
		optional: sql.NullString{String: "", Valid: id%2 == 0},
		payload:  append([]byte{0, byte(id), 0xff, 0x80, '\n'}, value...),
	}
}

func check(ctx context.Context, store database, expected manifest) (report, error) {
	options := sql.TxOptions{ReadOnly: true, Isolation: store.isolation}
	transaction, err := store.connection.BeginTx(ctx, &options)
	if err != nil {
		return report{}, fmt.Errorf("begin check: %w", err)
	}
	defer transaction.Rollback()
	return checkTransaction(ctx, transaction, store, expected)
}

func checkTransaction(ctx context.Context, transaction *sql.Tx, store database, expected manifest) (report, error) {
	var count, version int
	var dataset string
	var checkpoint int64
	if err := transaction.QueryRowContext(ctx, "SELECT COUNT(*), COALESCE(MIN(version), 0), COALESCE(MIN(dataset), ''), COALESCE(MIN(checkpoint), 0) FROM borq_probe_state").Scan(&count, &version, &dataset, &checkpoint); err != nil {
		return report{}, fmt.Errorf("read fixture state: %w", err)
	}
	if count != 1 || version != 2 {
		return report{}, fmt.Errorf("invalid fixture state: rows=%d fixture_version=%d", count, version)
	}
	if dataset != expected.Dataset {
		return report{}, fmt.Errorf("dataset mismatch: expected %q, got %q", expected.Dataset, dataset)
	}
	if checkpoint != expected.Checkpoint {
		return report{}, fmt.Errorf("checkpoint mismatch: expected %d, got %d", expected.Checkpoint, checkpoint)
	}
	values, err := checkRecords(ctx, transaction, expected)
	if err != nil {
		return report{}, err
	}
	var databaseVersion string
	if err := transaction.QueryRowContext(ctx, store.versionQuery).Scan(&databaseVersion); err != nil {
		return report{}, fmt.Errorf("read database version: %w", err)
	}
	return report{Driver: store.driver, DatabaseVersion: databaseVersion, FixtureVersion: 2, Dataset: dataset, Checkpoint: checkpoint, Samples: values.samples, Rows: 128, Checksum: values.checksum}, nil
}

type checkedRecords struct {
	checksum string
	samples  []int64
}

func checkRecords(ctx context.Context, transaction *sql.Tx, expected manifest) (checkedRecords, error) {
	rows, err := transaction.QueryContext(ctx, "SELECT id, label, amount, optional_text, payload FROM borq_probe_records ORDER BY id")
	if err != nil {
		return checkedRecords{}, fmt.Errorf("read fixture rows: %w", err)
	}
	defer rows.Close()
	fingerprint := sha256.New()
	encoder := json.NewEncoder(fingerprint)
	count := 0
	var samples []int64
	for rows.Next() {
		var actual record
		if err := rows.Scan(&actual.id, &actual.label, &actual.amount, &actual.optional, &actual.payload); err != nil {
			return checkedRecords{}, fmt.Errorf("read fixture value: %w", err)
		}
		count++
		wanted := expectedRecord(expected, count)
		if actual.id != wanted.id || actual.label != wanted.label || actual.amount != wanted.amount || actual.optional != wanted.optional || !bytes.Equal(actual.payload, wanted.payload) {
			return checkedRecords{}, fmt.Errorf("seed data mismatch at row %d (stored id %d)", count, actual.id)
		}
		if len(samples) < 3 {
			samples = append(samples, actual.amount)
		}
		if err := encoder.Encode([]any{actual.id, actual.label, actual.amount, actual.optional, actual.payload}); err != nil {
			return checkedRecords{}, fmt.Errorf("fingerprint row: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return checkedRecords{}, fmt.Errorf("iterate fixture rows: %w", err)
	}
	if count != 128 {
		return checkedRecords{}, fmt.Errorf("seed row count mismatch: expected 128, got %d", count)
	}
	return checkedRecords{checksum: hex.EncodeToString(fingerprint.Sum(nil)), samples: samples}, nil
}

func advance(ctx context.Context, store database, previous, next manifest) (report, error) {
	transaction, err := beginWrite(ctx, store)
	if err != nil {
		return report{}, err
	}
	defer transaction.Rollback()
	var checkpoint int64
	if err := transaction.QueryRowContext(ctx, "SELECT checkpoint FROM borq_probe_state WHERE id = 1").Scan(&checkpoint); err != nil {
		return report{}, fmt.Errorf("read current checkpoint: %w", err)
	}
	// A retry after a lost commit response must verify the already committed next values.
	if checkpoint == next.Checkpoint {
		return checkTransaction(ctx, transaction, store, next)
	}
	if _, err := checkTransaction(ctx, transaction, store, previous); err != nil {
		return report{}, fmt.Errorf("refusing to advance damaged or unexpected data: %w", err)
	}
	update, err := transaction.PrepareContext(ctx, "UPDATE borq_probe_records SET label=$1, amount=$2, optional_text=$3, payload=$4 WHERE id=$5")
	if err != nil {
		return report{}, err
	}
	defer update.Close()
	for id := 1; id <= 128; id++ {
		value := expectedRecord(next, id)
		if _, err := update.ExecContext(ctx, value.label, value.amount, value.optional, value.payload, value.id); err != nil {
			return report{}, fmt.Errorf("advance row %d: %w", id, err)
		}
	}
	if _, err := transaction.ExecContext(ctx, "UPDATE borq_probe_state SET checkpoint=$1 WHERE id=1", next.Checkpoint); err != nil {
		return report{}, fmt.Errorf("advance checkpoint: %w", err)
	}
	result, err := checkTransaction(ctx, transaction, store, next)
	if err != nil {
		return report{}, err
	}
	if err := transaction.Commit(); err != nil {
		return report{}, fmt.Errorf("commit checkpoint: %w", err)
	}
	return result, nil
}
