package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/stefanmcshane/borq/api/appapi"
)

func newManagementBackupStatusCommand() *cobra.Command {
	scope := applicationScope{}
	output := "cli"
	command := &cobra.Command{Use: "status", Short: "Show missing, disabled, stale or failed backups for every database.", Args: cobra.NoArgs}
	command.Long = "Inspect primary backup archives and show offsite export progress separately. Uses your saved Borq login. Available archive metadata does not replace a restore test."
	addApplicationServerFlag(command, &scope)
	command.Flags().StringVar(&output, "output", output, "Output format: cli or json.")
	command.RunE = func(command *cobra.Command, arguments []string) error {
		if output != "cli" && output != "json" {
			return errors.New("--output must be cli or json")
		}
		client, err := applicationClient(scope.server)
		if err != nil {
			return err
		}
		response, err := client.GetManagementBackupCoverage(command.Context())
		coverage, err := readManagedResponse[appapi.ManagementBackupCoverage](response, err)
		if err != nil {
			return err
		}
		if output == "json" {
			return json.NewEncoder(command.OutOrStdout()).Encode(coverage)
		}
		writer := tabwriter.NewWriter(command.OutOrStdout(), 0, 4, 2, ' ', 0)
		if _, err := fmt.Fprintln(writer, "TYPE\tDATABASE\tSTATUS\tLATEST BACKUP (UTC)"); err != nil {
			return err
		}
		for _, component := range coverage.Components {
			last := "none observed"
			if component.LastSuccess != nil {
				last = component.LastSuccess.UTC().Format(time.RFC3339)
			}
			if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n", component.Kind, component.Name, component.State, last); err != nil {
				return err
			}
		}
		if err := writer.Flush(); err != nil {
			return err
		}
		for _, component := range coverage.Components {
			if string(component.State) != "available" {
				if _, err := fmt.Fprintf(command.OutOrStdout(), "\n%s %s: %s\n", component.Kind, component.Name, component.Message); err != nil {
					return err
				}
			}
		}
		_, err = fmt.Fprintln(command.OutOrStdout(), "\nOffsite export success does not confirm that every database has a backup. Available means archive evidence was found; recovery must be tested separately.")
		return err
	}
	return command
}
