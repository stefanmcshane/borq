package offsite

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/stefanmcshane/borq/foundations/objectstore"
)

// RunInput captures one source through the same executable's private exporter command.
type RunInput struct {
	// Repository contains independently retained repository access.
	Repository Repository
	// Source resolves source storage credentials only for this attempt.
	Source objectstore.Config
	// Inventory was frozen before Checkpoint was captured.
	Inventory FrozenInventory
	// Checkpoint is retained with this source, including under source-scoped pruning.
	Checkpoint VaultCheckpoint
	// Executable is the installed Borq executable, resolved by the runtime boundary.
	Executable string
	// CacheDir holds persistent private Restic metadata only, never a source-object mirror.
	CacheDir string
	// KeepLast is the number of successful snapshots retained for this source.
	KeepLast int
}

func resticCommand(ctx context.Context, repository Repository, args ...string) *exec.Cmd {
	arguments := append([]string{"--cache-dir", repository.cacheDir}, args...)
	command := exec.CommandContext(ctx, "restic", arguments...)
	command.Env = []string{"RESTIC_REPOSITORY=" + repository.URL, "RESTIC_PASSWORD=" + repository.Password, "AWS_ACCESS_KEY_ID=" + repository.AccessKey, "AWS_SECRET_ACCESS_KEY=" + repository.SecretKey, "AWS_SESSION_TOKEN=" + repository.SessionToken, "AWS_DEFAULT_REGION=" + repository.Region, "AWS_REGION=" + repository.Region, "AWS_EC2_METADATA_DISABLED=true"}
	return command
}
func run(ctx context.Context, repository Repository, args ...string) ([]byte, error) {
	command := resticCommand(ctx, repository, args...)
	if err := configureProcess(command); err != nil {
		return nil, err
	}
	defer terminateProcess(command)
	var output limitedBuffer
	command.Stdout = &output
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var status *exec.ExitError
		if errors.As(err, &status) {
			return nil, fmt.Errorf("restic %s failed with exit code %d", args[0], status.ExitCode())
		}
		return nil, errors.New("restic executable could not run")
	}
	return output.Bytes(), nil
}

// Run creates a snapshot only when the exporter succeeds, then applies source-scoped retention.
// The snapshot identity is returned even if subsequent retention fails.
func Run(ctx context.Context, input RunInput) (string, error) {
	if err := input.Repository.Validate(); err != nil {
		return "", err
	}
	if err := ValidateSeparation(input.Repository, input.Source); err != nil {
		return "", err
	}
	if input.Executable == "" || input.KeepLast < 1 {
		return "", errors.New("offsite executable and positive retention are required")
	}
	config := ExportInput{Storage: storageConfig(input.Source), Inventory: input.Inventory, Checkpoint: input.Checkpoint}
	if err := config.validate(); err != nil {
		return "", err
	}
	cache, cleanup, err := prepareCache(input.CacheDir)
	if err != nil {
		return "", err
	}
	defer cleanup()
	input.Repository.cacheDir = cache
	if _, err := run(ctx, input.Repository, "cat", "config"); err != nil {
		if !strings.HasSuffix(err.Error(), "exit code 10") {
			return "", err
		}
		if _, err := run(ctx, input.Repository, "init"); err != nil {
			return "", err
		}
	}
	directory, err := os.MkdirTemp("", "borq-offsite-")
	if err != nil {
		return "", errors.New("create private exporter configuration")
	}
	defer os.RemoveAll(directory)
	source := input.Source
	data, err := json.Marshal(config)
	if err != nil {
		return "", errors.New("encode private exporter configuration")
	}
	file := filepath.Join(directory, "source.json")
	if err := os.WriteFile(file, data, 0600); err != nil {
		return "", errors.New("write private exporter configuration")
	}
	sourceID := SourceID(source)
	output, err := run(ctx, input.Repository, "backup", "--json", "--quiet", "--host", "borq-management", "--tag", "borq-source-"+sourceID, "--stdin-filename", "objects.tar", "--stdin-from-command", "--", input.Executable, "management", "backups", "export", "--source-config", file)
	if err != nil {
		return "", err
	}
	summary := struct {
		SnapshotID string `json:"snapshot_id"`
	}{}
	for _, line := range bytes.Split(output, []byte("\n")) {
		var item struct {
			SnapshotID string `json:"snapshot_id"`
		}
		if json.Unmarshal(line, &item) == nil && item.SnapshotID != "" {
			summary = item
		}
	}
	if summary.SnapshotID == "" {
		return "", errors.New("restic backup completed without a snapshot identity")
	}
	_, err = run(ctx, input.Repository, "forget", "--host", "borq-management", "--tag", "borq-source-"+sourceID, "--group-by", "host,tags", "--keep-last", strconv.Itoa(input.KeepLast), "--prune", "--quiet")
	return summary.SnapshotID, err
}

// List returns completed snapshots from exactly one managed source.
func List(ctx context.Context, repository Repository, source string) ([]Snapshot, error) {
	if err := repository.Validate(); err != nil {
		return nil, err
	}
	if !identity(source) {
		return nil, errors.New("offsite source identity must be 32 lowercase hexadecimal characters")
	}
	if repository.cacheDir == "" {
		cache, cleanup, err := prepareCache("")
		if err != nil {
			return nil, err
		}
		defer cleanup()
		repository.cacheDir = cache
	}
	output, err := run(ctx, repository, "snapshots", "--json", "--host", "borq-management", "--tag", "borq-source-"+source)
	if err != nil {
		return nil, err
	}
	snapshots := []Snapshot{}
	if json.Unmarshal(output, &snapshots) != nil {
		return nil, errors.New("restic snapshot list is invalid")
	}
	return snapshots, nil
}

// Recover requires a listed source-scoped snapshot and validates both dump exit and imported bytes.
func Recover(ctx context.Context, config RecoveryConfig, snapshot string) (Archive, error) {
	ctx, cancelRecovery := context.WithTimeout(ctx, 24*time.Hour)
	defer cancelRecovery()
	cache, cleanup, err := prepareCache("")
	if err != nil {
		return Archive{}, err
	}
	defer cleanup()
	config.Repository.cacheDir = cache
	snapshots, err := List(ctx, config.Repository, config.Source)
	if err != nil {
		return Archive{}, err
	}
	selected := ""
	for _, candidate := range snapshots {
		if candidate.ID == snapshot || snapshot == "latest" {
			selected = candidate.ID
		}
	}
	if selected == "" {
		return Archive{}, errors.New("snapshot does not belong to the selected source")
	}
	dumpCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	command := resticCommand(dumpCtx, config.Repository, "dump", selected, "/objects.tar")
	if err := configureProcess(command); err != nil {
		return Archive{}, err
	}
	defer terminateProcess(command)
	output, err := command.StdoutPipe()
	if err != nil {
		return Archive{}, errors.New("open restic recovery stream")
	}
	if err := command.Start(); err != nil {
		return Archive{}, errors.New("start restic recovery")
	}
	archive, restoreErr := Restore(dumpCtx, config.Destination.Config(), config.Source, output)
	if restoreErr != nil {
		cancel()
	}
	producerErr := command.Wait()
	if restoreErr != nil {
		return Archive{}, restoreErr
	}
	if producerErr != nil {
		return Archive{}, errors.New("restic recovery producer failed; destination remains unpublished")
	}
	return archive, nil
}

type limitedBuffer struct{ bytes.Buffer }

func (output *limitedBuffer) Write(data []byte) (int, error) {
	if output.Len()+len(data) > 4<<20 {
		return 0, errors.New("restic output exceeds 4 MiB")
	}
	return output.Buffer.Write(data)
}

// ReadPrivateConfig decodes one small operator-owned file without allowing ambient credentials.
func ReadPrivateConfig(file string, destination any) error {
	return readPrivateConfig(file, destination, 2<<20)
}

func readPrivateConfig(file string, destination any, limit int64) error {
	opened, err := os.Open(file)
	if err != nil {
		return errors.New("open private recovery configuration")
	}
	defer opened.Close()
	info, err := opened.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() < 0 || info.Size() > limit {
		return fmt.Errorf("recovery configuration must be a private regular file of at most %d MiB (mode 0600)", limit>>20)
	}
	decoder := json.NewDecoder(io.LimitReader(opened, limit+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return errors.New("invalid private recovery configuration")
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("recovery configuration contains trailing data")
	}
	return nil
}

func prepareCache(directory string) (string, func(), error) {
	if directory == "" {
		temporary, err := os.MkdirTemp("", "borq-restic-cache-")
		if err != nil {
			return "", nil, errors.New("create private Restic metadata cache")
		}
		return temporary, func() { _ = os.RemoveAll(temporary) }, nil
	}
	if !filepath.IsAbs(directory) {
		return "", nil, errors.New("Restic metadata cache directory must be absolute")
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return "", nil, errors.New("create private Restic metadata cache")
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return "", nil, errors.New("Restic metadata cache must be a private directory")
	}
	return directory, func() {}, nil
}
