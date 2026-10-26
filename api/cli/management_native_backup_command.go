package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"filippo.io/age"
	"github.com/spf13/cobra"
	"github.com/stefanmcshane/borq/business/managementbackup"
	"github.com/stefanmcshane/borq/business/offsite"
)

func newManagementLocalBackupsCommand() *cobra.Command {
	command := &cobra.Command{Use: "local", Short: "List and recover encrypted native management snapshots from local or rehydrated S3.", Args: cobra.NoArgs}
	keyFile := ""
	keygen := &cobra.Command{Use: "keygen", Short: "Write a private age recovery identity and print its public backup recipient.", Args: cobra.NoArgs, RunE: func(command *cobra.Command, arguments []string) error {
		identity, err := age.GenerateX25519Identity()
		if err != nil {
			return errors.New("generate management recovery identity")
		}
		file, err := os.OpenFile(keyFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return errors.New("identity destination must be a new private file")
		}
		_, writeErr := fmt.Fprintln(file, identity.String())
		closeErr := file.Close()
		if errors.Join(writeErr, closeErr) != nil {
			return errors.New("save independently retained private identity")
		}
		_, err = fmt.Fprintln(command.OutOrStdout(), identity.Recipient().String())
		return err
	}}
	keygen.Flags().StringVar(&keyFile, "identity-file", "", "New private identity file retained outside Borq.")
	_ = keygen.MarkFlagRequired("identity-file")
	file := ""
	command.PersistentFlags().StringVar(&file, "config", "", "Private recovery JSON with local S3 access, age identity and OpenBao recovery material.")
	list := &cobra.Command{Use: "list", Short: "List complete native artifacts without the original SQLite or vault.", Args: cobra.NoArgs, RunE: func(command *cobra.Command, arguments []string) error {
		var config offsite.RecoveryConfig
		if err := offsite.ReadPrivateConfig(file, &config); err != nil {
			return err
		}
		entries, err := managementbackup.List(command.Context(), config.Local.Config())
		if err != nil {
			return err
		}
		artifacts := []managementbackup.Artifact{}
		for _, entry := range entries {
			artifacts = append(artifacts, entry.Artifact)
		}
		return json.NewEncoder(command.OutOrStdout()).Encode(artifacts)
	}}
	dataDir := ""
	restore := &cobra.Command{Use: "restore KIND ID", Short: "Restore SQLite to a fresh paused data directory or native Raft state into a fresh vault.", Args: cobra.ExactArgs(2), RunE: func(command *cobra.Command, arguments []string) error {
		var config offsite.RecoveryConfig
		if err := offsite.ReadPrivateConfig(file, &config); err != nil {
			return err
		}
		if arguments[0] == "sqlite" && dataDir == "" {
			return errors.New("SQLite restore requires --data-dir for a fresh destination")
		}
		if err := managementbackup.Restore(command.Context(), managementbackup.RestoreInput{Storage: config.Local.Config(), Kind: arguments[0], ID: arguments[1], Identity: config.Identity, DataDir: dataDir, OpenBao: config.OpenBao}); err != nil {
			return err
		}
		_, err := fmt.Fprintln(command.OutOrStdout(), "native component restored; keep management workers paused until storage, vault and cluster connections have been reviewed")
		return err
	}}
	restore.Flags().StringVar(&dataDir, "data-dir", "", "Fresh SQLite recovery directory; startup remains blocked until explicit resume.")
	reviewDir := ""
	inspect := &cobra.Command{Use: "inspect", Short: "Read retained operation states while recovered management workers remain paused.", Args: cobra.NoArgs, RunE: func(command *cobra.Command, arguments []string) error {
		operations, err := managementbackup.Inspect(command.Context(), reviewDir)
		if err != nil {
			return err
		}
		return json.NewEncoder(command.OutOrStdout()).Encode(operations)
	}}
	inspect.Flags().StringVar(&reviewDir, "data-dir", "", "Recovered data directory.")
	_ = inspect.MarkFlagRequired("data-dir")
	bindDir := ""
	rebind := &cobra.Command{Use: "rebind", Short: "Rebind recovered SQLite and vault storage credentials to the private configuration's local S3.", Args: cobra.NoArgs, RunE: func(command *cobra.Command, arguments []string) error {
		var config offsite.RecoveryConfig
		if err := offsite.ReadPrivateConfig(file, &config); err != nil {
			return err
		}
		return managementbackup.Rebind(command.Context(), bindDir, config.Local.Config(), config.OpenBao)
	}}
	rebind.Flags().StringVar(&bindDir, "data-dir", "", "Paused recovered data directory.")
	_ = rebind.MarkFlagRequired("data-dir")
	resumeDir := ""
	resume := &cobra.Command{Use: "resume", Short: "Explicitly permit restored operation replay after reviewing connections and queued work.", Args: cobra.NoArgs, RunE: func(command *cobra.Command, arguments []string) error { return managementbackup.Resume(resumeDir) }}
	resume.Flags().StringVar(&resumeDir, "data-dir", "", "Reviewed recovered data directory.")
	_ = resume.MarkFlagRequired("data-dir")
	command.AddCommand(keygen, list, restore, inspect, rebind, resume)
	return command
}
