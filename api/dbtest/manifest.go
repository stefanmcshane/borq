package dbtest

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type manifest struct {
	// FormatVersion identifies the external expectation format.
	FormatVersion int `json:"format_version"`
	// Dataset identifies the CI scenario independently of its database.
	Dataset string `json:"dataset"`
	// Checkpoint selects the exact committed generation expected after recovery.
	Checkpoint int64 `json:"checkpoint"`
	// Seed reproduces the random test values without consulting the database.
	Seed string `json:"seed"`
}

func prepareManifest(path, dataset string, checkpoint int64) (manifest, error) {
	if path == "" || dataset == "" || len(dataset) > 256 || strings.ContainsRune(dataset, 0) || checkpoint < 1 {
		return manifest{}, errors.New("prepare requires --manifest, a dataset and a positive checkpoint")
	}
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		return manifest{}, fmt.Errorf("generate expected seed: %w", err)
	}
	expected := manifest{FormatVersion: 1, Dataset: dataset, Checkpoint: checkpoint, Seed: hex.EncodeToString(seed)}
	file, err := os.CreateTemp(filepath.Dir(path), ".db-test-manifest-*")
	if err != nil {
		return manifest{}, fmt.Errorf("create expected manifest: %w", err)
	}
	defer os.Remove(file.Name())
	defer file.Close()
	// Test values contain no credentials; read permission permits a non-root read-only mount.
	if err := file.Chmod(0644); err != nil {
		return manifest{}, err
	}
	if err := json.NewEncoder(file).Encode(expected); err != nil {
		return manifest{}, err
	}
	if err := file.Sync(); err != nil {
		return manifest{}, err
	}
	if err := file.Close(); err != nil {
		return manifest{}, err
	}
	// Publish a complete file without ever replacing an earlier recovery expectation.
	if err := os.Link(file.Name(), path); err != nil {
		return manifest{}, fmt.Errorf("publish expected manifest (existing files are never replaced): %w", err)
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return manifest{}, err
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return manifest{}, err
	}
	return expected, nil
}

func readManifest(path string) (manifest, error) {
	if path == "" {
		return manifest{}, errors.New("--manifest or DB_TEST_MANIFEST is required; expected data must be retained outside the database pod")
	}
	file, err := os.Open(path)
	if err != nil {
		return manifest{}, fmt.Errorf("read expected manifest: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return manifest{}, fmt.Errorf("inspect expected manifest: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > 65536 {
		return manifest{}, errors.New("expected manifest must be a regular file of at most 64 KiB")
	}
	decoder := json.NewDecoder(io.LimitReader(file, 65537))
	decoder.DisallowUnknownFields()
	var expected manifest
	if err := decoder.Decode(&expected); err != nil {
		return manifest{}, fmt.Errorf("decode expected manifest: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return manifest{}, errors.New("expected manifest must contain exactly one JSON object")
	}
	seed, err := hex.DecodeString(expected.Seed)
	if expected.FormatVersion != 1 || expected.Dataset == "" || len(expected.Dataset) > 256 || strings.ContainsRune(expected.Dataset, 0) || expected.Checkpoint < 1 || err != nil || len(seed) != 32 {
		return manifest{}, errors.New("invalid expected manifest format, dataset, checkpoint or seed")
	}
	return expected, nil
}
