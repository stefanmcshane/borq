package cli

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/stefanmcshane/borq/business/cli/management"
)

func newManagementKeysCommand(cfg Config) *cobra.Command {
	input := management.ExportKeysInput{OutputFile: "borq-management-recovery.json", Timeout: 5 * time.Minute}
	command := &cobra.Command{
		Use:   "keys",
		Short: "Save Borq and OpenBao administrator keys to a private recovery file.",
		Long:  "Read the existing keys from your management cluster using its kubeconfig. You can run this command as often as needed. It does not create or rotate keys. If setup is still starting, it waits for the keys until --timeout. Keep the private output file in secure storage outside the cluster. Existing incomplete or different files are never overwritten.",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, arguments []string) error {
			if command.Flags().Changed("kubeconfig") && strings.TrimSpace(input.KubeconfigPath) == "" {
				return errors.New("--kubeconfig must name a kubeconfig file; omit the flag to use the current Kubernetes context")
			}
			selection, cleanup, err := selectManagementKubeconfig(input.KubeconfigPath, cfg)
			if err != nil {
				return err
			}
			defer cleanup()
			exportInput := input
			exportInput.KubeconfigPath = selection.path
			_, _ = fmt.Fprintf(command.ErrOrStderr(), "Using Kubernetes context %q. Waiting for management keys (up to %s)...\n", selection.contextName, input.Timeout)
			file, err := management.ExportKeys(command.Context(), exportInput)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "Management keys saved to: %s\nThis file contains the Borq administrator token and OpenBao's administrator token and auto-unseal key. The auto-unseal key also recovers management backup encryption; no separate backup key file is needed.\nSave a copy in secure storage outside your cluster, such as a password manager. Keep it private and out of Git.\n\nStart private access in another terminal:\nkubectl%s -n borq-mgmt port-forward service/borq 7833:80 --address 127.0.0.1\n\nLog in once; only the Borq administrator token is saved to the CLI:\nborq login --api-url http://127.0.0.1:7833 --token-file %s\n\nAfter login and verifying your secure backup, remove the working copy of the recovery file.\nApply your management configuration using your saved login:\nborq management apply -f management.yaml\nAfter configuring OIDC, use your organization account for normal access.\n", file, managementKubeconfigFlag(selection.flagPath), quoteLoginArgument(file))
			return err
		},
	}
	command.Flags().StringVar(&input.KubeconfigPath, "kubeconfig", "", "Management cluster kubeconfig; defaults to the current context from KUBECONFIG or ~/.kube/config. No Borq login is required.")
	command.Flags().StringVar(&input.OutputFile, "output-file", input.OutputFile, "New private file for both Borq and OpenBao recovery keys.")
	command.Flags().DurationVar(&input.Timeout, "timeout", input.Timeout, "Maximum wait for reconciliation to make the keys available.")
	return command
}

func newManagementBootstrapFromRestoreCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "bootstrap-from-restore",
		Short: "Restore a management installation from backup (not implemented yet).",
		Long:  "Not implemented yet. The planned flow retrieves the OpenBao backup using independently retained storage credentials and the offsite password, restores OpenBao with the saved management keys, then recovers RustFS archives and Borq's database before resuming reconciliation. Current restore tooling may require a temporary S3 service to retrieve the vault snapshot. This command makes no changes.",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, arguments []string) error {
			return errors.New("management bootstrap-from-restore is not implemented yet; no resources have been changed")
		},
	}
}
