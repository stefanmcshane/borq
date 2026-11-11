// Package dbtest runs stable database fixtures for managed addon upgrade and recovery checks.
package dbtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/spf13/cobra"
)

type config struct {
	driver          string `env:"DB_TEST_DRIVER"`
	dataset         string `env:"DB_TEST_DATASET"`
	manifestPath    string `env:"DB_TEST_MANIFEST"`
	expected        manifest
	next            manifest
	databasePath    string `env:"SQLITE_DATABASE_PATH"`
	databaseUrl     string `env:"DATABASE_URL"`
	interval        time.Duration
	timeout         time.Duration
	requireExisting bool
}

// NewCommand creates the shared fixture CLI, selecting SQLite or Postgres through --driver.
// Cobra requires a pointer to maintain command and flag state.
func NewCommand() *cobra.Command {
	return newCommand(config{
		driver:       os.Getenv("DB_TEST_DRIVER"),
		dataset:      os.Getenv("DB_TEST_DATASET"),
		manifestPath: os.Getenv("DB_TEST_MANIFEST"),
		databasePath: os.Getenv("SQLITE_DATABASE_PATH"),
		databaseUrl:  os.Getenv("DATABASE_URL"),
	})
}

func newCommand(settings config) *cobra.Command {
	var database string
	var nextPath string
	checkpoint := int64(1)
	root := &cobra.Command{
		Use:           "db-test",
		Short:         "Seed and verify data across database upgrades and disaster recovery",
		SilenceErrors: true,
		SilenceUsage:  true,
		Args:          cobra.NoArgs,
	}
	root.PersistentFlags().StringVar(&settings.driver, "driver", settings.driver, "Database driver: sqlite or postgres (DB_TEST_DRIVER)")
	root.PersistentFlags().StringVar(&settings.manifestPath, "manifest", settings.manifestPath, "Expected-data file retained by CI outside the database pod (DB_TEST_MANIFEST)")
	root.PersistentFlags().StringVar(&database, "database", "", "SQLite file path or Postgres URL (defaults to the engine's environment variable)")
	root.PersistentFlags().DurationVar(&settings.interval, "log-interval", 10*time.Second, "Time between read-only checks while serving")
	root.PersistentFlags().DurationVar(&settings.timeout, "timeout", 30*time.Second, "Timeout for each database operation")
	root.PersistentFlags().BoolVar(&settings.requireExisting, "require-existing", false, "Refuse initialization even for seed; serve and check always require existing data")
	run := func(command *cobra.Command, action string) error {
		if action == "prepare" {
			expected, err := prepareManifest(settings.manifestPath, settings.dataset, checkpoint)
			if err != nil {
				return err
			}
			return json.NewEncoder(command.OutOrStdout()).Encode(expected)
		}
		if settings.driver != "sqlite" && settings.driver != "postgres" {
			return errors.New("set --driver or DB_TEST_DRIVER to sqlite or postgres")
		}
		var err error
		settings.expected, err = readManifest(settings.manifestPath)
		if err != nil {
			return err
		}
		if action == "advance" {
			settings.next, err = readManifest(nextPath)
			if err != nil {
				return err
			}
			if settings.next.Dataset != settings.expected.Dataset || settings.next.Checkpoint != settings.expected.Checkpoint+1 || settings.next.Seed == settings.expected.Seed {
				return errors.New("next manifest must have the same dataset, the next checkpoint and a new random seed")
			}
		}
		if database != "" {
			settings.databasePath, settings.databaseUrl = database, database
		}
		if settings.interval <= 0 || settings.timeout <= 0 {
			return errors.New("log interval and timeout must be positive")
		}
		return execute(command, action, settings)
	}
	root.RunE = func(command *cobra.Command, args []string) error { return run(command, "serve") }
	for _, action := range []struct{ name, description string }{
		{"prepare", "Create immutable random expectations on the CI runner without opening a database"},
		{"serve", "Verify existing data, then keep running and logging read-only checks (default)"},
		{"seed", "Create the fixture only when absent; never repair existing data"},
		{"check", "Compare every seeded value without modifying the database"},
		{"advance", "Verify the current checkpoint and atomically write the next manifest's random values"},
	} {
		child := &cobra.Command{
			Use:   action.name,
			Short: action.description,
			Args:  cobra.NoArgs,
			RunE:  func(command *cobra.Command, args []string) error { return run(command, action.name) },
		}
		if action.name == "prepare" {
			child.Flags().StringVar(&settings.dataset, "dataset", settings.dataset, "CI scenario identity (DB_TEST_DATASET)")
			child.Flags().Int64Var(&checkpoint, "checkpoint", 1, "Expected checkpoint number")
		}
		if action.name == "advance" {
			child.Flags().StringVar(&nextPath, "next-manifest", "", "Already prepared, separately retained next checkpoint manifest")
		}
		root.AddCommand(child)
	}
	return root
}

func execute(command *cobra.Command, action string, settings config) error {
	database, err := openDatabase(action, settings)
	if err != nil {
		return err
	}
	defer database.connection.Close()
	ctx := command.Context()
	if action == "advance" {
		operation, cancel := context.WithTimeout(ctx, settings.timeout)
		defer cancel()
		result, err := advance(operation, database, settings.expected, settings.next)
		if err != nil {
			return err
		}
		return json.NewEncoder(command.OutOrStdout()).Encode(result)
	}
	if action == "seed" {
		operation, cancel := context.WithTimeout(ctx, settings.timeout)
		err := initialize(operation, database, settings)
		cancel()
		if err != nil {
			return err
		}
	}
	if action != "serve" {
		operation, cancel := context.WithTimeout(ctx, settings.timeout)
		defer cancel()
		result, err := check(operation, database, settings.expected)
		if err != nil {
			return err
		}
		return json.NewEncoder(command.OutOrStdout()).Encode(result)
	}
	log := slog.New(slog.NewJSONHandler(command.ErrOrStderr(), nil)).With("driver", settings.driver, "dataset", settings.expected.Dataset, "checkpoint", settings.expected.Checkpoint)
	ticker := time.NewTicker(settings.interval)
	defer ticker.Stop()
	started := false
	for {
		operation, cancel := context.WithTimeout(ctx, settings.timeout)
		result, err := check(operation, database, settings.expected)
		cancel()
		if ctx.Err() != nil {
			log.Info("stopped")
			return nil
		}
		if err != nil {
			if !started {
				return fmt.Errorf("initial check failed: %w", err)
			}
			// Keep the container available for exec and diagnosis after a failed check.
			log.Error("check failed", "error", err)
		} else {
			log.Info("check passed", "database_version", result.DatabaseVersion, "rows", result.Rows, "checksum", result.Checksum)
		}
		started = true
		select {
		case <-ctx.Done():
			log.Info("stopped")
			return nil
		case <-ticker.C:
		}
	}
}
