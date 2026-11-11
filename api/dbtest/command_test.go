package dbtest

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/matryer/is"
	_ "modernc.org/sqlite"
)

// TestSqliteWorkload owns the fixture's durable lifecycle and loss detection on real SQLite files.
func TestSqliteWorkload(t *testing.T) {
	runWorkloadSuite(t, "sqlite", func(t *testing.T) string {
		return filepath.Join(t.TempDir(), "probe.sqlite")
	})
}

func runWorkloadSuite(t *testing.T, engine string, newAddress func(*testing.T) string) {
	t.Helper()
	t.Run("seed close reopen and repeat", func(t *testing.T) {
		assert := is.New(t)
		address := newAddress(t)
		output, err := runCommand(t.Context(), engine, address, "seed")
		assert.NoErr(err)
		seeded := decodeReport(t, output)
		assert.Equal(seeded.Driver, engine)
		assert.Equal(seeded.Dataset, "borq-db-test-v1")
		assert.Equal(seeded.FixtureVersion, 2)
		assert.Equal(seeded.Checkpoint, int64(1))
		assert.Equal(seeded.Rows, 128)
		assert.True(seeded.DatabaseVersion != "")
		assert.Equal(seeded.Samples, []int64{-5490368709433951712, 4548633042200005678, -2679962678618469187})
		assert.Equal(seeded.Checksum, "fba2fc358760e956e9828cf1e0f8739f717b90e5a3933497bbd10ed254eef8a6")
		// Each command closes its connection; subsequent commands reopen the durable database.
		for _, action := range []string{"check", "seed"} {
			output, err = runCommand(t.Context(), engine, address, action, "--require-existing")
			assert.NoErr(err)
			assert.Equal(decodeReport(t, output), seeded)
		}
		database := openTestDatabase(t, engine, address)
		var label string
		var amount int64
		var optional sql.NullString
		var payload []byte
		assert.NoErr(database.QueryRow("SELECT label, amount, optional_text, payload FROM borq_probe_records WHERE id = 1").Scan(&label, &amount, &optional, &payload))
		assert.Equal(label, "borq-db-test-v1:checkpoint-1:row-001: café 日本語 'quoted'\nsecond line")
		assert.Equal(amount, int64(-5490368709433951712))
		assert.Equal(optional.Valid, false)
		wantedPayload, err := hex.DecodeString("0001ff800a98636a8107760bc19e7a1681481d33fc037510f8019468e4faf7a9057c6e9ef1")
		assert.NoErr(err)
		assert.Equal(payload, wantedPayload)
		assert.NoErr(database.Close())
	})
	t.Run("recovery verifies the expected dataset", func(t *testing.T) {
		assert := is.New(t)
		address := newAddress(t)
		output, err := runCommand(t.Context(), engine, address, "seed")
		assert.NoErr(err)
		seeded := decodeReport(t, output)
		assert.Equal(seeded.Dataset, "borq-db-test-v1")
		for _, action := range []string{"check", "seed"} {
			_, err = runCommand(t.Context(), engine, address, action, "--manifest", filepath.Join("testdata", "wrong.json"))
			assert.True(err != nil && strings.Contains(err.Error(), "dataset mismatch"))
		}
		output, err = runCommand(t.Context(), engine, address, "check")
		assert.NoErr(err)
		assert.Equal(decodeReport(t, output), seeded)
	})
	t.Run("damage is detected and never reseeded", func(t *testing.T) {
		for _, damage := range []struct{ name, query, message string }{
			{"last row missing", "DELETE FROM borq_probe_records WHERE id = 128", "row count mismatch"},
			{"middle row missing", "DELETE FROM borq_probe_records WHERE id = 64", "data mismatch"},
			{"text changed", "UPDATE borq_probe_records SET label = 'damaged' WHERE id = 1", "data mismatch"},
			{"number changed", "UPDATE borq_probe_records SET amount = 0 WHERE id = 1", "data mismatch"},
			{"null became empty", "UPDATE borq_probe_records SET optional_text = '' WHERE id = 1", "data mismatch"},
			{"empty became null", "UPDATE borq_probe_records SET optional_text = NULL WHERE id = 2", "data mismatch"},
			{"binary changed", "UPDATE borq_probe_records SET payload = (SELECT payload FROM borq_probe_records WHERE id = 2) WHERE id = 1", "data mismatch"},
			{"extra row", "INSERT INTO borq_probe_records SELECT 129, label, amount, optional_text, payload FROM borq_probe_records WHERE id = 1", "data mismatch"},
			{"state missing", "DELETE FROM borq_probe_state", "invalid fixture state"},
			{"unknown version", "UPDATE borq_probe_state SET version = 99", "invalid fixture state"},
			{"table missing", "DROP TABLE borq_probe_records", "read fixture rows"},
		} {
			t.Run(damage.name, func(t *testing.T) {
				assert := is.New(t)
				address := newAddress(t)
				_, err := runCommand(t.Context(), engine, address, "seed")
				assert.NoErr(err)
				database := openTestDatabase(t, engine, address)
				_, err = database.Exec(damage.query)
				assert.NoErr(err)
				assert.NoErr(database.Close())
				_, err = runCommand(t.Context(), engine, address, "check")
				assert.True(err != nil && strings.Contains(err.Error(), damage.message))
				_, err = runCommand(t.Context(), engine, address, "seed")
				assert.True(err != nil)
				_, err = runCommand(t.Context(), engine, address, "advance", "--next-manifest", filepath.Join("testdata", "next.json"))
				assert.True(err != nil)
				_, err = runCommand(t.Context(), engine, address, "check")
				assert.True(err != nil && strings.Contains(err.Error(), damage.message))
			})
		}
	})
	t.Run("failed initialization rolls back schema creation", func(t *testing.T) {
		assert := is.New(t)
		address := newAddress(t)
		database := openTestDatabase(t, engine, address)
		_, err := database.Exec("CREATE VIEW borq_probe_records AS SELECT 1 AS id")
		assert.NoErr(err)
		_, err = runCommand(t.Context(), engine, address, "seed")
		assert.True(err != nil && strings.Contains(err.Error(), "create fixture"))
		_, err = database.Exec("SELECT version FROM borq_probe_state")
		assert.True(err != nil)
		_, err = database.Exec("DROP VIEW borq_probe_records")
		assert.NoErr(err)
		assert.NoErr(database.Close())
		_, err = runCommand(t.Context(), engine, address, "seed")
		assert.NoErr(err)
	})
	t.Run("checkpoint updates are atomic and idempotent", func(t *testing.T) {
		assert := is.New(t)
		address := newAddress(t)
		output, err := runCommand(t.Context(), engine, address, "seed")
		assert.NoErr(err)
		previous := decodeReport(t, output)
		database := openTestDatabase(t, engine, address)
		guard := "CREATE TRIGGER reject_checkpoint BEFORE UPDATE ON borq_probe_state BEGIN SELECT RAISE(ABORT, 'reject checkpoint'); END"
		removeGuard := "DROP TRIGGER reject_checkpoint"
		if engine == "postgres" {
			guard = "ALTER TABLE borq_probe_state ADD CONSTRAINT reject_checkpoint CHECK (checkpoint = 1)"
			removeGuard = "ALTER TABLE borq_probe_state DROP CONSTRAINT reject_checkpoint"
		}
		_, err = database.Exec(guard)
		assert.NoErr(err)
		_, err = runCommand(t.Context(), engine, address, "advance", "--next-manifest", filepath.Join("testdata", "next.json"))
		assert.True(err != nil && strings.Contains(err.Error(), "advance checkpoint"))
		output, err = runCommand(t.Context(), engine, address, "check")
		assert.NoErr(err)
		assert.Equal(decodeReport(t, output), previous)
		_, err = database.Exec(removeGuard)
		assert.NoErr(err)
		assert.NoErr(database.Close())
		output, err = runCommand(t.Context(), engine, address, "advance", "--next-manifest", filepath.Join("testdata", "next.json"))
		assert.NoErr(err)
		advanced := decodeReport(t, output)
		assert.Equal(advanced.Checkpoint, int64(2))
		assert.True(advanced.Checksum != previous.Checksum)
		assert.True(advanced.Samples[0] != previous.Samples[0])
		output, err = runCommand(t.Context(), engine, address, "advance", "--next-manifest", filepath.Join("testdata", "next.json"))
		assert.NoErr(err)
		assert.Equal(decodeReport(t, output), advanced)
		_, err = runCommand(t.Context(), engine, address, "advance", "--next-manifest", filepath.Join("testdata", "conflicting-next.json"))
		assert.True(err != nil && strings.Contains(err.Error(), "data mismatch"))
		_, err = runCommand(t.Context(), engine, address, "check")
		assert.True(err != nil && strings.Contains(err.Error(), "checkpoint mismatch"))
		output, err = runCommand(t.Context(), engine, address, "check", "--manifest", filepath.Join("testdata", "next.json"))
		assert.NoErr(err)
		assert.Equal(decodeReport(t, output), advanced)
	})
	t.Run("concurrent initialization checks and checkpoint updates", func(t *testing.T) {
		assert := is.New(t)
		address := newAddress(t)
		for _, action := range []string{"seed", "check", "advance"} {
			results := make(chan error, 2)
			for range 2 {
				go func() {
					args := []string{action}
					if action == "advance" {
						args = append(args, "--next-manifest", filepath.Join("testdata", "next.json"))
					}
					_, err := runCommand(t.Context(), engine, address, args...)
					results <- err
				}()
			}
			for range 2 {
				assert.NoErr(<-results)
			}
		}
		_, err := runCommand(t.Context(), engine, address, "check", "--manifest", filepath.Join("testdata", "next.json"))
		assert.NoErr(err)
	})
	t.Run("server stays available after damage and stops on cancellation", func(t *testing.T) {
		assert := is.New(t)
		address := newAddress(t)
		_, err := runCommand(t.Context(), engine, address, "seed")
		assert.NoErr(err)
		for start := range 2 {
			ctx, cancel := context.WithCancel(t.Context())
			command := newCommand(config{driver: engine, manifestPath: filepath.Join("testdata", "baseline.json")})
			command.SetArgs([]string{"serve", "--database", address, "--log-interval=20ms"})
			command.SetContext(ctx)
			logs := &logBuffer{}
			command.SetErr(logs)
			finished := make(chan error, 1)
			go func() { finished <- command.Execute() }()
			t.Cleanup(cancel)
			waitForLog(t, logs, "check passed")
			if start == 1 {
				database := openTestDatabase(t, engine, address)
				_, err := database.Exec("DELETE FROM borq_probe_records WHERE id = 128")
				assert.NoErr(err)
				assert.NoErr(database.Close())
				waitForLog(t, logs, "check failed")
				select {
				case err := <-finished:
					t.Fatalf("server exited after damage: %v", err)
				default:
				}
			}
			cancel()
			select {
			case err := <-finished:
				assert.NoErr(err)
			case <-time.After(5 * time.Second):
				t.Fatal("server did not stop after cancellation")
			}
		}
	})
}

// TestSqliteMissingDatabase ensures checks and recovery-mode startup cannot conceal total volume loss.
func TestSqliteMissingDatabase(t *testing.T) {
	assert := is.New(t)
	for _, args := range [][]string{{"check"}, {"advance", "--next-manifest", filepath.Join("testdata", "next.json")}, {"serve"}, {"serve", "--require-existing"}} {
		address := filepath.Join(t.TempDir(), "missing.sqlite")
		_, err := runCommand(t.Context(), "sqlite", address, args...)
		assert.True(err != nil)
		_, err = os.Stat(address)
		assert.True(os.IsNotExist(err))
	}
	address := filepath.Join(t.TempDir(), "empty.sqlite")
	assert.NoErr(os.WriteFile(address, nil, 0660))
	_, err := runCommand(t.Context(), "sqlite", address, "seed", "--require-existing")
	assert.True(err != nil && strings.Contains(err.Error(), "forbids seeding"))
}

// TestCommandConfiguration protects exec ergonomics and keeps connection secrets out of help and errors.
func TestCommandConfiguration(t *testing.T) {
	assert := is.New(t)
	t.Setenv("DATABASE_URL", "postgres://user:secret-do-not-print@localhost/test")
	t.Setenv("DB_TEST_DRIVER", "postgres")
	command := NewCommand()
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetArgs([]string{"--help"})
	assert.NoErr(command.Execute())
	assert.True(!strings.Contains(output.String(), "secret-do-not-print"))
	for _, test := range []struct {
		engine, address string
		args            []string
		message         string
	}{
		{"sqlite", "", []string{"check"}, "SQLITE_DATABASE_PATH"},
		{"postgres", "", []string{"check"}, "DATABASE_URL"},
		{"sqlite", "unused", []string{"check", "--timeout=0s"}, "must be positive"},
		{"other", "unused", []string{"check"}, "--driver"},
		{"sqlite", "unused", []string{"migrate"}, "unknown command"},
		{"sqlite", "unused", []string{"check", "extra"}, "unknown command"},
	} {
		_, err := runCommand(t.Context(), test.engine, test.address, test.args...)
		assert.True(err != nil && strings.Contains(err.Error(), test.message))
	}
	path := filepath.Join(t.TempDir(), "from-env.sqlite")
	t.Setenv("SQLITE_DATABASE_PATH", path)
	t.Setenv("DB_TEST_DRIVER", "sqlite")
	t.Setenv("DB_TEST_MANIFEST", filepath.Join("testdata", "baseline.json"))
	command = NewCommand()
	output.Reset()
	command.SetOut(&output)
	command.SetArgs([]string{"seed"})
	assert.NoErr(command.ExecuteContext(t.Context()))
	assert.Equal(decodeReport(t, output.String()).Dataset, "borq-db-test-v1")
	_, err := os.Stat(path)
	assert.NoErr(err)
}

// TestPostgresConnectionBoundary guards secret redaction and operation timeouts before SQL is available.
func TestPostgresConnectionBoundary(t *testing.T) {
	for _, address := range []string{
		"postgres://user:secret-do-not-print@localhost:bad-port/test",
		"postgres://user:secret-do-not-print@bad%host/test",
	} {
		assert := is.New(t)
		output, err := runCommand(t.Context(), "postgres", address, "check")
		assert.True(err != nil && strings.Contains(err.Error(), "configure Postgres"))
		assert.True(!strings.Contains(err.Error(), "secret-do-not-print"))
		assert.Equal(output, "")
	}
	for _, sslMode := range []string{"disable", "require"} {
		t.Run("stalled startup ssl="+sslMode, func(t *testing.T) {
			assert := is.New(t)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			assert.NoErr(err)
			t.Cleanup(func() { _ = listener.Close() })
			accepted := make(chan net.Conn, 1)
			go func() {
				connection, err := listener.Accept()
				if err == nil {
					accepted <- connection
				}
			}()
			finished := make(chan error, 1)
			go func() {
				address := "postgres://user:password@" + listener.Addr().String() + "/test?sslmode=" + sslMode
				_, err := runCommand(t.Context(), "postgres", address, "check", "--timeout=50ms")
				finished <- err
			}()
			select {
			case connection := <-accepted:
				t.Cleanup(func() { _ = connection.Close() })
			case <-time.After(2 * time.Second):
				t.Fatal("command did not reach the stalled Postgres connection")
			}
			select {
			case err := <-finished:
				assert.True(err != nil)
			case <-time.After(2 * time.Second):
				t.Fatal("Postgres startup exceeded the operation timeout")
			}
		})
	}
}

func runCommand(ctx context.Context, engine, address string, args ...string) (string, error) {
	command := newCommand(config{driver: engine, manifestPath: filepath.Join("testdata", "baseline.json")})
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	hasManifest := false
	for _, argument := range args {
		if argument == "--manifest" || strings.HasPrefix(argument, "--manifest=") {
			hasManifest = true
		}
	}
	if !hasManifest {
		args = append(args, "--manifest", filepath.Join("testdata", "baseline.json"))
	}
	command.SetArgs(append(args, "--driver", engine, "--database", address))
	err := command.ExecuteContext(ctx)
	return output.String(), err
}

func openTestDatabase(t *testing.T, engine, address string) *sql.DB {
	t.Helper()
	driver := engine
	if engine == "sqlite" {
		address += "?_pragma=busy_timeout(5000)"
	}
	database, err := sql.Open(driver, address)
	is.New(t).NoErr(err)
	t.Cleanup(func() { _ = database.Close() })
	return database
}

type logBuffer struct {
	mutex  sync.Mutex
	buffer bytes.Buffer
}

// Write captures logs safely while the test observes the running server.
func (buffer *logBuffer) Write(value []byte) (int, error) {
	buffer.mutex.Lock()
	defer buffer.mutex.Unlock()
	return buffer.buffer.Write(value)
}

func (buffer *logBuffer) contains(value string) bool {
	buffer.mutex.Lock()
	defer buffer.mutex.Unlock()
	return strings.Contains(buffer.buffer.String(), value)
}

func waitForLog(t *testing.T, logs *logBuffer, value string) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for !logs.contains(value) {
		select {
		case <-deadline.C:
			t.Fatal(fmt.Sprintf("server did not log %q", value))
		case <-ticker.C:
		}
	}
}

func decodeReport(t *testing.T, output string) report {
	t.Helper()
	var result report
	is.New(t).NoErr(json.Unmarshal([]byte(output), &result))
	return result
}

// TestSqliteSnapshotRecovery verifies the same fixture after recovering a real SQLite snapshot with the source removed.
func TestSqliteSnapshotRecovery(t *testing.T) {
	assert := is.New(t)
	address := filepath.Join(t.TempDir(), "source.sqlite")
	snapshot := filepath.Join(t.TempDir(), "restored.sqlite")
	output, err := runCommand(t.Context(), "sqlite", address, "seed", "--manifest", filepath.Join("testdata", "recovery.json"))
	assert.NoErr(err)
	before := decodeReport(t, output)
	database := openTestDatabase(t, "sqlite", address)
	_, err = database.Exec("VACUUM INTO $1", snapshot)
	assert.NoErr(err)
	assert.NoErr(database.Close())
	output, err = runCommand(t.Context(), "sqlite", address, "advance", "--manifest", filepath.Join("testdata", "recovery.json"), "--next-manifest", filepath.Join("testdata", "recovery-next.json"))
	assert.NoErr(err)
	assert.Equal(decodeReport(t, output).Checkpoint, int64(2))
	assert.NoErr(os.Remove(address))
	_, err = runCommand(t.Context(), "sqlite", address, "check", "--manifest", filepath.Join("testdata", "recovery.json"))
	assert.True(err != nil)
	_, err = runCommand(t.Context(), "sqlite", snapshot, "check", "--manifest", filepath.Join("testdata", "recovery-next.json"))
	assert.True(err != nil && strings.Contains(err.Error(), "checkpoint mismatch"))
	output, err = runCommand(t.Context(), "sqlite", snapshot, "check", "--manifest", filepath.Join("testdata", "recovery.json"))
	assert.NoErr(err)
	assert.Equal(decodeReport(t, output), before)
}
