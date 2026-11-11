package cli

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/stefanmcshane/borq/business/managementbackup"
	"github.com/stefanmcshane/borq/business/offsite"
)

func newManagementLocalBackupsCommand() *cobra.Command {
	command := &cobra.Command{Use: "local", Short: "List and recover encrypted native management snapshots from local or rehydrated S3.", Args: cobra.NoArgs}
	file := ""
	command.PersistentFlags().StringVar(&file, "config", "", "Private recovery JSON with local S3 access and the exported OpenBao recovery material.")
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
	keysFile, outputFile := "", ""
	decrypt := &cobra.Command{Use: "decrypt KIND ID", Short: "Verify and decrypt one native snapshot to a new private file for manual recovery.", Args: cobra.ExactArgs(2), RunE: func(command *cobra.Command, arguments []string) error {
		if file == "" {
			return errors.New("native decrypt requires --config with private S3 access")
		}
		var config offsite.RecoveryConfig
		if err := offsite.ReadPrivateConfig(file, &config); err != nil {
			return err
		}
		var keys struct {
			Borq struct {
				AdminToken string `json:"adminToken"`
			} `json:"borq"`
			OpenBao managementbackup.OpenBaoRecovery `json:"openbao"`
		}
		if err := offsite.ReadPrivateConfig(keysFile, &keys); err != nil {
			return err
		}
		if _, err := managementbackup.DecryptSnapshot(command.Context(), managementbackup.DecryptInput{Storage: config.Local.Config(), Kind: arguments[0], ID: arguments[1], SealKey: keys.OpenBao.SealKey, OutputFile: outputFile}); err != nil {
			return err
		}
		_, err := fmt.Fprintf(command.OutOrStdout(), "Verified native snapshot saved to %s. Keep it private; no services were restored or started.\n", outputFile)
		return err
	}}
	decrypt.Flags().StringVar(&keysFile, "keys-file", "", "Original private combined file saved by management keys; no separate backup identity is required.")
	decrypt.Flags().StringVar(&outputFile, "output", "", "New mode-0600 file for the authenticated native snapshot; existing paths and symlinks are refused.")
	_ = decrypt.MarkFlagRequired("keys-file")
	_ = decrypt.MarkFlagRequired("output")
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
	spaceDirectory, spaceManifest := "", ""
	generations := map[string]string{}
	rebindSpace := &cobra.Command{Use: "rebind-space SPACE_ID", Short: "Reconnect a recovered space bucket and its selected SQLite generations before startup.", Args: cobra.ExactArgs(1), RunE: func(command *cobra.Command, arguments []string) error {
		var config offsite.RecoveryConfig
		if err := offsite.ReadPrivateConfig(file, &config); err != nil {
			return err
		}
		manifest, err := offsite.ReadArchiveManifest(spaceManifest)
		if err != nil {
			return err
		}
		objects, err := offsite.SelectSpaceObjects(manifest, arguments[0], generations)
		if err != nil {
			return err
		}
		return managementbackup.RebindSpace(command.Context(), spaceDirectory, managementbackup.SpaceRecovery{SpaceID: arguments[0], Source: manifest.Source, Storage: config.Local.Config(), Objects: objects, OpenBao: config.OpenBao})
	}}
	rebindSpace.Flags().StringVar(&spaceDirectory, "data-dir", "", "Paused recovered management data directory.")
	rebindSpace.Flags().StringVar(&spaceManifest, "manifest", "", "Completion manifest saved by offsite restore --manifest.")
	rebindSpace.Flags().StringToStringVar(&generations, "generations", nil, "Explicit application=generation selections when captured chronology is ambiguous.")
	_ = rebindSpace.MarkFlagRequired("data-dir")
	_ = rebindSpace.MarkFlagRequired("manifest")
	resume := &cobra.Command{Use: "resume", Short: "Explicitly permit restored operation replay after reviewing connections and queued work.", Args: cobra.NoArgs, RunE: func(command *cobra.Command, arguments []string) error { return managementbackup.Resume(resumeDir) }}
	resume.Flags().StringVar(&resumeDir, "data-dir", "", "Reviewed recovered data directory.")
	_ = resume.MarkFlagRequired("data-dir")
	command.AddCommand(newManagementDevelopmentBackupsCommand(), list, restore, decrypt, inspect, rebind, rebindSpace, resume)
	return command
}
