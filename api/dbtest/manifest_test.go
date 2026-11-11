package dbtest

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matryer/is"
)

// TestPrepareOffline preserves durable external expectations without depending on a live database or pod.
func TestPrepareOffline(t *testing.T) {
	assert := is.New(t)
	directory := t.TempDir()
	path := filepath.Join(directory, "checkpoint-7.json")
	databasePath := filepath.Join(directory, "offline.sqlite")
	command := newCommand(config{driver: "sqlite", databasePath: databasePath})
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetArgs([]string{"prepare", "--manifest", path, "--dataset=ci-run", "--checkpoint=7"})
	assert.NoErr(command.ExecuteContext(t.Context()))
	var expected manifest
	assert.NoErr(json.Unmarshal(output.Bytes(), &expected))
	assert.Equal(expected.FormatVersion, 1)
	assert.Equal(expected.Dataset, "ci-run")
	assert.Equal(expected.Checkpoint, int64(7))
	assert.Equal(len(expected.Seed), 64)
	retained, err := os.ReadFile(path)
	assert.NoErr(err)
	assert.Equal(retained, output.Bytes())
	_, err = os.Stat(databasePath)
	assert.True(os.IsNotExist(err))
	command = newCommand(config{})
	command.SetArgs([]string{"prepare", "--manifest", path, "--dataset=ci-run", "--checkpoint=8"})
	err = command.ExecuteContext(t.Context())
	assert.True(err != nil && strings.Contains(err.Error(), "never replaced"))
	unchanged, err := os.ReadFile(path)
	assert.NoErr(err)
	assert.Equal(unchanged, retained)
	otherPath := filepath.Join(directory, "another.json")
	command = newCommand(config{})
	command.SetOut(&output)
	command.SetArgs([]string{"prepare", "--manifest", otherPath, "--dataset=ci-run", "--checkpoint=7"})
	assert.NoErr(command.ExecuteContext(t.Context()))
	other, err := readManifest(otherPath)
	assert.NoErr(err)
	assert.True(other.Seed != expected.Seed)
	_, err = os.Stat(databasePath + "-wal")
	assert.True(os.IsNotExist(err))
}

// TestMissingOrInvalidExpectations fails before opening or seeding a database when CI's artifact is unavailable.
func TestMissingOrInvalidExpectations(t *testing.T) {
	for _, test := range []struct{ name, contents, message string }{
		{"missing path", "", "--manifest"},
		{"lost artifact", "", "read expected manifest"},
		{"partial json", "{", "decode expected manifest"},
		{"bad fields", `{"format_version":1,"dataset":"run","checkpoint":0,"seed":"bad"}`, "invalid expected manifest"},
		{"unknown field", `{"unexpected":true}`, "decode expected manifest"},
		{"extra json", `{} {}`, "exactly one JSON object"},
		{"oversized", strings.Repeat(" ", 65537), "at most 64 KiB"},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			directory := t.TempDir()
			path := filepath.Join(directory, "expected.json")
			if test.contents != "" {
				assert.NoErr(os.WriteFile(path, []byte(test.contents), 0644))
			}
			if test.name == "missing path" {
				path = ""
			}
			databasePath := filepath.Join(directory, "missing.sqlite")
			command := newCommand(config{driver: "sqlite", manifestPath: path, databasePath: databasePath})
			command.SetArgs([]string{"seed"})
			err := command.ExecuteContext(t.Context())
			assert.True(err != nil && strings.Contains(err.Error(), test.message))
			_, err = os.Stat(databasePath)
			assert.True(os.IsNotExist(err))
		})
	}
}

// TestAdvanceExpectationGuards prevents checkpoint regressions or identity changes before database access.
func TestAdvanceExpectationGuards(t *testing.T) {
	for _, test := range []struct {
		name          string
		checkpoint    int64
		dataset, seed string
	}{
		{"skipped checkpoint", 3, "borq-db-test-v1", strings.Repeat("20", 32)},
		{"another scenario", 2, "another-run", strings.Repeat("20", 32)},
		{"reused seed", 2, "borq-db-test-v1", "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			nextPath := filepath.Join(t.TempDir(), "next.json")
			next := manifest{FormatVersion: 1, Dataset: test.dataset, Checkpoint: test.checkpoint, Seed: test.seed}
			contents, err := json.Marshal(next)
			assert.NoErr(err)
			assert.NoErr(os.WriteFile(nextPath, contents, 0644))
			command := newCommand(config{driver: "postgres"})
			command.SetArgs([]string{"advance", "--manifest", filepath.Join("testdata", "baseline.json"), "--next-manifest", nextPath})
			err = command.ExecuteContext(t.Context())
			assert.True(err != nil && strings.Contains(err.Error(), "next manifest"))
		})
	}
}
