package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/stefanmcshane/borq/business/offsite"
)

func newManagementBackupsCommand() *cobra.Command {
	command := &cobra.Command{Use: "backups", Short: "List and recover encrypted PostgreSQL S3 archives using private external configuration.", Args: cobra.NoArgs}
	recoveryFile := ""
	list := &cobra.Command{Use: "list", Short: "List completed snapshots without the original management cluster.", Args: cobra.NoArgs, RunE: func(command *cobra.Command, arguments []string) error {
		var config offsite.RecoveryConfig
		if err := offsite.ReadPrivateConfig(recoveryFile, &config); err != nil {
			return err
		}
		snapshots, err := offsite.List(command.Context(), config.Repository, config.Source)
		if err != nil {
			return err
		}
		return json.NewEncoder(command.OutOrStdout()).Encode(snapshots)
	}}
	list.Flags().StringVar(&recoveryFile, "config", "", "Private recovery JSON file retained outside Borq.")
	_ = list.MarkFlagRequired("config")
	restoreFile := ""
	restore := &cobra.Command{Use: "restore SNAPSHOT", Short: "Rehydrate a snapshot into a fresh unpublished S3 prefix.", Args: cobra.ExactArgs(1), RunE: func(command *cobra.Command, arguments []string) error {
		var config offsite.RecoveryConfig
		if err := offsite.ReadPrivateConfig(restoreFile, &config); err != nil {
			return err
		}
		manifest, err := offsite.Recover(command.Context(), config, arguments[0])
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(command.OutOrStdout(), "restored %d verified objects into %s/%s; source %s\n", len(manifest.Objects), config.Destination.Bucket, config.Destination.Prefix, manifest.Source)
		return err
	}}
	restore.Flags().StringVar(&restoreFile, "config", "", "Private recovery JSON file with fresh destination credentials.")
	_ = restore.MarkFlagRequired("config")
	sourceFile := ""
	exporter := &cobra.Command{Use: "export", Hidden: true, Args: cobra.NoArgs, RunE: func(command *cobra.Command, arguments []string) error {
		var source offsite.StorageConfig
		if err := offsite.ReadPrivateConfig(sourceFile, &source); err != nil {
			return err
		}
		return offsite.Export(command.Context(), source.Config(), command.OutOrStdout())
	}}
	exporter.Flags().StringVar(&sourceFile, "source-config", "", "Private ephemeral source configuration.")
	_ = exporter.MarkFlagRequired("source-config")
	command.AddCommand(list, restore, exporter)
	return command
}
