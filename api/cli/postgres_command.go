package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/stefanmcshane/borq/api/appapi"
	yamlreader "gopkg.in/yaml.v3"
	"sigs.k8s.io/yaml"
)

type postgresOptions struct {
	scope                                                       applicationScope
	action, name, file, backup, target, history, search, output string
	wait                                                        bool
	timeout                                                     time.Duration
	limit, instances                                            int
	storage, image, snapshots                                   string
}

func newManagedAddonCommand() *cobra.Command {
	group := &cobra.Command{Use: "managed", Short: "Manage automatically protected services.", Args: cobra.NoArgs}
	group.AddCommand(newPostgresCommand())
	return group
}

func newPostgresCommand() *cobra.Command {
	group := &cobra.Command{Use: "postgres", Short: "Manage PostgreSQL databases and recovery.", Args: cobra.NoArgs}
	for _, action := range []string{"create", "apply", "list", "get", "delete", "env", "logs", "metrics", "operation"} {
		group.AddCommand(newPostgresAction(action))
	}
	backups := &cobra.Command{Use: "backup", Short: "Browse backups and restore a new database.", Args: cobra.NoArgs}
	backups.AddCommand(newPostgresAction("backup-list"), newPostgresAction("backup-restore"))
	group.AddCommand(backups)
	return group
}

func newPostgresAction(action string) *cobra.Command {
	options := postgresOptions{action: action}
	use := action + " NAME"
	args := cobra.ExactArgs(1)
	if action == "list" {
		use = action
		args = cobra.NoArgs
	}
	if action == "apply" {
		use = "apply [NAME]"
		args = cobra.MaximumNArgs(1)
	}
	if action == "backup-list" {
		use = "list SOURCE"
	}
	if action == "backup-restore" {
		use = "restore NEW_NAME"
	}
	command := &cobra.Command{Use: use, Short: "PostgreSQL " + strings.ReplaceAll(action, "-", " ") + ".", Args: args, RunE: func(command *cobra.Command, arguments []string) error {
		if len(arguments) > 0 {
			options.name = arguments[0]
		}
		return executePostgres(command, options)
	}}
	addApplicationServerFlag(command, &options.scope)
	if action != "operation" {
		command.Flags().StringVar(&options.scope.organization, "organization", "", "Organization owning the space.")
		command.Flags().StringVar(&options.scope.space, "space", "", "Space owning the managed database.")
	}
	if action == "create" || action == "apply" {
		command.Flags().IntVar(&options.instances, "instances", 1, "PostgreSQL instance count.")
		command.Flags().StringVar(&options.storage, "storage", "1Gi", "Persistent storage per instance.")
		command.Flags().StringVar(&options.image, "image", "", "Pinned PostgreSQL image; omitted uses the tested default.")
		command.Flags().StringVar(&options.snapshots, "snapshots", "preferred", "CSI protection: preferred, required, or disabled.")
	}
	if action == "apply" {
		command.Flags().StringVarP(&options.file, "file", "f", "", "Space fragment with apps, customAddons and managedAddons.")
	}
	if action == "create" || action == "apply" || action == "delete" || action == "backup-restore" {
		command.Flags().BoolVar(&options.wait, "wait", true, "Wait for the accepted operation to complete.")
		command.Flags().DurationVar(&options.timeout, "timeout", 15*time.Minute, "Maximum local wait; server work continues after timeout.")
	}
	if action == "backup-restore" {
		command.Flags().StringVar(&options.backup, "backup", "", "Approved opaque backup ID from backup list.")
		command.Flags().StringVar(&options.target, "recovery-target", "", "Optional RFC3339 recovery time within the catalogue window.")
		_ = command.MarkFlagRequired("backup")
	}
	if action == "logs" || action == "metrics" {
		command.Flags().StringVar(&options.history, "range", "1h", "History window: 30m, 1h, 12h, or 3d.")
	}
	if action == "logs" {
		command.Flags().StringVar(&options.search, "search", "", "Literal log substring.")
		command.Flags().IntVar(&options.limit, "limit", 200, "Maximum newest lines, from 1 to 1000.")
	}
	if action == "env" {
		command.Flags().StringVarP(&options.output, "output", "o", "env", "Credential output: env, json, or yaml.")
	}
	return command
}

func executePostgres(command *cobra.Command, options postgresOptions) error {
	if options.action != "operation" {
		if err := validateApplicationScope(options.scope); err != nil {
			return err
		}
	}
	if options.wait && options.timeout <= 0 {
		return errors.New("--timeout must be greater than zero")
	}
	client, err := applicationClient(options.scope.server)
	if err != nil {
		return err
	}
	ctx := command.Context()
	org, space, name := options.scope.organization, options.scope.space, options.name
	switch options.action {
	case "list":
		response, err := client.ListPostgres(ctx, org, space)
		value, err := readManagedResponse[[]appapi.Postgres](response, err)
		if err != nil {
			return err
		}
		return printApplicationValue(command.OutOrStdout(), value)
	case "get":
		response, err := client.GetPostgres(ctx, org, space, name)
		value, err := readManagedResponse[appapi.Postgres](response, err)
		if err != nil {
			return err
		}
		return printApplicationValue(command.OutOrStdout(), value)
	case "operation":
		response, err := client.GetPostgresOperation(ctx, name)
		value, err := readManagedResponse[appapi.Operation](response, err)
		if err != nil {
			return err
		}
		return printApplicationValue(command.OutOrStdout(), value)
	case "env":
		if options.output != "env" && options.output != "json" && options.output != "yaml" {
			return errors.New("--output must be env, json, or yaml")
		}
		response, err := client.GetPostgresEnv(ctx, org, space, name)
		value, err := readManagedResponse[appapi.PostgresEnv](response, err)
		if err != nil {
			return err
		}
		if options.output == "json" {
			return json.NewEncoder(command.OutOrStdout()).Encode(value)
		}
		if options.output == "yaml" {
			return printApplicationValue(command.OutOrStdout(), value)
		}
		for _, entry := range []struct{ key, value string }{{"PGHOST", value.PGHOST}, {"PGPORT", value.PGPORT}, {"PGDATABASE", value.PGDATABASE}, {"PGUSER", value.PGUSER}, {"PGPASSWORD", value.PGPASSWORD}, {"DATABASE_URL", value.DATABASEURL}} {
			if _, err := fmt.Fprintf(command.OutOrStdout(), "%s='%s'\n", entry.key, strings.ReplaceAll(entry.value, "'", "'\"'\"'")); err != nil {
				return err
			}
		}
		return nil
	case "backup-list":
		response, err := client.ListPostgresBackups(ctx, org, space, name)
		value, err := readManagedResponse[[]appapi.PostgresBackup](response, err)
		if err != nil {
			return err
		}
		return printApplicationValue(command.OutOrStdout(), value)
	case "metrics":
		history := appapi.GetPostgresMetricsParamsRange(options.history)
		response, err := client.GetPostgresMetrics(ctx, org, space, name, &appapi.GetPostgresMetricsParams{Range: &history})
		value, err := readManagedResponse[appapi.PostgresMetrics](response, err)
		if err != nil {
			return err
		}
		return printApplicationValue(command.OutOrStdout(), value)
	case "logs":
		history := appapi.GetPostgresLogsParamsRange(options.history)
		response, err := client.GetPostgresLogs(ctx, org, space, name, &appapi.GetPostgresLogsParams{Range: &history, Search: &options.search, Limit: &options.limit})
		value, err := readManagedResponse[appapi.AppLogs](response, err)
		if err != nil {
			return err
		}
		return printApplicationValue(command.OutOrStdout(), value)
	case "apply":
		if options.file != "" {
			if name != "" {
				return errors.New("provide a NAME or --file, not both")
			}
			var body appapi.SpaceConfiguration
			if err := readManagedDocument(options.file, "SpaceConfiguration", &body); err != nil {
				return err
			}
			response, err := client.ApplySpaceConfiguration(ctx, org, space, body)
			operation, err := readManagedResponse[appapi.Operation](response, err)
			if err != nil {
				return err
			}
			return finishManagedOperation(command, client, operation, options.wait, options.timeout, true)
		}
		if name == "" {
			return errors.New("provide NAME or --file")
		}
	}
	var response *http.Response
	switch options.action {
	case "create", "apply":
		body := appapi.PostgresInput{}
		if command.Flags().Changed("instances") {
			body.Instances = &options.instances
		}
		if command.Flags().Changed("storage") {
			body.Storage = &options.storage
		}
		if command.Flags().Changed("image") {
			body.Image = &options.image
		}
		if command.Flags().Changed("snapshots") {
			value := appapi.PostgresInputSnapshots(options.snapshots)
			body.Snapshots = &value
		}
		apply := options.action == "apply"
		response, err = client.PutPostgres(ctx, org, space, name, &appapi.PutPostgresParams{Apply: &apply}, body)
	case "delete":
		response, err = client.DeletePostgres(ctx, org, space, name)
	case "backup-restore":
		body := appapi.PostgresRestoreInput{BackupID: options.backup}
		if options.target != "" {
			target, parseError := time.Parse(time.RFC3339, options.target)
			if parseError != nil {
				return errors.New("--recovery-target must be RFC3339")
			}
			body.RecoveryTarget = &target
		}
		response, err = client.RestorePostgres(ctx, org, space, name, body)
	default:
		return errors.New("unsupported managed PostgreSQL action")
	}
	value, err := readManagedResponse[appapi.Postgres](response, err)
	if err != nil {
		return err
	}
	if !options.wait {
		return printApplicationValue(command.OutOrStdout(), value)
	}
	return finishManagedOperation(command, client, appapi.Operation{ID: value.Operation}, true, options.timeout, false)
}

// HTTP response pointers are owned by net/http and closed after bounded decoding.
func readManagedResponse[Output any](response *http.Response, err error) (Output, error) {
	var value Output
	if err != nil {
		return value, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return value, err
	}
	if response.StatusCode >= 400 {
		return value, applicationFailure(response.StatusCode, body)
	}
	if err := json.Unmarshal(body, &value); err != nil {
		return value, fmt.Errorf("decode management response: %w", err)
	}
	return value, nil
}

// Cobra and the generated client require pointers for output and HTTP connection state.
func finishManagedOperation(command *cobra.Command, client *appapi.ClientWithResponses, operation appapi.Operation, wait bool, timeout time.Duration, management bool) error {
	if !wait {
		return printApplicationValue(command.OutOrStdout(), operation)
	}
	ctx, cancel := context.WithTimeout(command.Context(), timeout)
	defer cancel()
	for {
		if operation.Status == appapi.Succeeded {
			return printApplicationValue(command.OutOrStdout(), operation)
		}
		if operation.Status == appapi.Failed || operation.Status == appapi.Canceled {
			return fmt.Errorf("operation %s %s: %s", operation.ID, operation.Status, operation.Message)
		}
		var response *http.Response
		var err error
		if management {
			response, err = client.GetManagementOperation(ctx, operation.ID)
		} else {
			response, err = client.GetPostgresOperation(ctx, operation.ID)
		}
		operation, err = readManagedResponse[appapi.Operation](response, err)
		if err != nil {
			return err
		}
		if operation.Status == appapi.Succeeded || operation.Status == appapi.Failed || operation.Status == appapi.Canceled {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func readManagedDocument(path, schema string, output any) error {
	source, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read configuration: %w", err)
	}
	return decodeManagedDocument(source, schema, output)
}

func decodeManagedDocument(source []byte, schema string, output any) error {
	decoder := yamlreader.NewDecoder(bytes.NewReader(source))
	var document any
	if err := decoder.Decode(&document); err != nil {
		return errors.New("configuration must contain one YAML object")
	}
	if _, ok := document.(map[string]any); !ok {
		return errors.New("configuration must contain one YAML object")
	}
	if err := decoder.Decode(&document); err != io.EOF {
		return errors.New("configuration must contain exactly one YAML document")
	}
	data, err := yaml.YAMLToJSONStrict(source)
	if err != nil {
		return errors.New("configuration must contain unique keys")
	}
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return errors.New("configuration must contain one JSON-compatible object")
	}
	specification, err := appapi.GetSwagger()
	if err != nil {
		return err
	}
	if err := specification.Components.Schemas[schema].Value.VisitJSON(value); err != nil {
		if message := appapi.SchemaValidationMessage(err); message != "" {
			return errors.New(message)
		}
		return errors.New("configuration does not match the OpenAPI configuration schema")
	}
	if err := json.Unmarshal(data, output); err != nil {
		return errors.New("configuration could not be decoded")
	}
	return nil
}
