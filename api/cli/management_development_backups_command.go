package cli

import (
	"github.com/spf13/cobra"
	"github.com/stefanmcshane/borq/api/appapi"
	"time"
)

func newManagementDevelopmentBackupsCommand() *cobra.Command {
	scope := applicationScope{}
	command := &cobra.Command{Use: "configure-dev", Hidden: true, Args: cobra.NoArgs, RunE: func(command *cobra.Command, arguments []string) error {
		return executeManagementDevelopmentBackups(command, scope)
	}}
	addApplicationServerFlag(command, &scope)
	return command
}

func executeManagementDevelopmentBackups(command *cobra.Command, scope applicationScope) error {
	client, err := applicationClient(scope.server)
	if err != nil {
		return err
	}
	response, err := client.GetManagementConfiguration(command.Context())
	configuration, err := readManagedResponse[appapi.ManagementConfiguration](response, err)
	if err != nil {
		return err
	}
	defaults := appapi.ManagementConfiguration{}
	if configuration.Backups == nil {
		interval, keepLast := "1h", 24
		defaults.Backups = &appapi.ManagementBackupInput{Enabled: true, Interval: &interval, KeepLast: &keepLast}
	}
	if configuration.Offsite == nil {
		defaults.Offsite = &appapi.ManagementOffsiteInput{Enabled: false}
	}
	if defaults.Backups == nil && defaults.Offsite == nil {
		return nil
	}
	response, err = client.ApplyManagementConfiguration(command.Context(), defaults)
	operation, err := readManagedResponse[appapi.Operation](response, err)
	if err != nil {
		return err
	}
	return finishManagedOperation(command, client, operation, true, 15*time.Minute, true)
}
