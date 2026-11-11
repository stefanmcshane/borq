package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/stefanmcshane/borq/api/appapi"
	"github.com/stefanmcshane/borq/business/cli/management"
	"github.com/stefanmcshane/borq/foundations/logger"
)

func newManagementCommand(ctx context.Context, log logger.Logger, cfg Config) *cobra.Command {
	command := &cobra.Command{
		Use:   "management",
		Short: "Operate the borq management cluster.",
		Args:  cobra.NoArgs,
		RunE:  func(command *cobra.Command, arguments []string) error { return command.Help() },
	}

	command.AddCommand(newManagementBootstrapCommand(ctx, log, cfg), newManagementKeysCommand(cfg), newManagementBootstrapFromRestoreCommand())
	command.AddCommand(newManagementApplyCommand(), newManagementExportCommand(), newManagementBackupsCommand(), newManagementSecretsCommand())
	command.AddCommand(newManagementServerCommand(log, cfg), newManagementDestroyCommand())

	return command
}

func newManagementBootstrapCommand(ctx context.Context, log logger.Logger, cfg Config) *cobra.Command {
	request := management.DefaultRequest()
	if !cfg.kubeconfigFromDefault {
		request.KubeconfigPath = cfg.KubeconfigPath
	}
	defaultImage := request.BorqImage
	// Leave image selection unset so a bundled image is used only when no image was requested.
	request.BorqImage = ""
	request.ProbeURL = ""
	request.Mode = ""
	request.ClusterName = ""
	development := false
	file := ""

	command := &cobra.Command{
		Use:   "bootstrap [CLUSTER]",
		Short: "Install Borq using the existing Kubernetes cluster name.",
		Long:  "Install Borq on existing Kubernetes with administrator-token access. Optionally supply management YAML with external OIDC credentials using --file. Deploy Pocket ID separately after bootstrap. The selected kubeconfig supplies the cluster name; reruns preserve the installed name. The optional cluster argument and --cluster-name override naming for a new installation.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			output, _ := cmd.Flags().GetString("output")
			if output != "cli" && output != "json" {
				return errors.New("--output must be cli or json")
			}
			var configuration appapi.ManagementConfiguration
			if cmd.Flags().Changed("file") {
				var err error
				configuration, err = readManagementBootstrapConfiguration(file)
				if err != nil {
					return err
				}
			}
			if len(args) == 1 {
				if cmd.Flags().Changed("cluster-name") && request.ClusterName != args[0] {
					return fmt.Errorf("cluster argument and --cluster-name differ; provide one cluster name")
				}
				request.ClusterName = args[0]
			}
			if cmd.Flags().Changed("development") {
				request.Mode = "prod"
				if development {
					request.Mode = "dev"
				}
			}
			executionContext := cmd.Context()
			if executionContext == nil {
				executionContext = ctx
			}

			if cmd.Flags().Changed("kubeconfig") && strings.TrimSpace(request.KubeconfigPath) == "" {
				return errors.New("--kubeconfig must name a kubeconfig file; omit the flag to use the current Kubernetes context")
			}
			selection, cleanup, err := selectManagementKubeconfig(request.KubeconfigPath, cfg)
			if err != nil {
				return err
			}
			defer cleanup()
			executionRequest := request
			executionRequest.KubeconfigPath = selection.path
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Using Kubernetes context %q.\n", selection.contextName)
			result, err := management.Execute(executionContext, log, executionRequest)
			if err != nil {
				return err
			}

			result.KubeconfigPath = selection.flagPath
			if output != "json" {
				if err := finishManagementBootstrap(cmd, result); err != nil {
					return err
				}
			}
			if cmd.Flags().Changed("file") {
				return applyManagementBootstrapConfiguration(cmd, result, configuration, request.Timeout)
			}
			if output == "json" {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
			}
			return nil
		},
	}

	command.Flags().StringVar(&request.ClusterName, "cluster-name", request.ClusterName, "Optional installation name override; defaults to the selected Kubernetes cluster and preserves installed naming on reruns.")
	command.Flags().DurationVar(&request.Timeout, "timeout", request.Timeout, "Overall bootstrap timeout.")
	command.Flags().StringVarP(&file, "file", "f", "", "Optional management YAML, including external OIDC; deploy Pocket ID after bootstrap.")
	command.Flags().String("output", "cli", "Output format: cli or json.")
	command.Flags().StringVar(&request.Namespace, "namespace", request.Namespace, "Management namespace (must be borq-mgmt).")
	command.Flags().StringVar(&request.KubeconfigPath, "kubeconfig", request.KubeconfigPath, "Existing cluster kubeconfig; defaults to the current context from KUBECONFIG or ~/.kube/config.")
	command.Flags().StringVar(&request.BorqImage, "server-image", "", "Borq server image reference or tag; an explicit image overrides automatic archive discovery.")
	command.Flags().Lookup("server-image").DefValue = defaultImage
	command.Flags().StringVar(&request.ServerImageArchivePath, "server-image-archive", "", "Offline server image archive for local kind or k3d; the bundled archive and cluster are detected automatically.")
	command.Flags().StringVar(&request.BorqImage, "borq-image", "", "Alias for --server-image.")
	command.Flags().Lookup("borq-image").DefValue = defaultImage

	command.Flags().BoolVar(&development, "development", false, "Use persistent single-node Raft and lightweight component defaults; inferred for local Docker clusters.")
	command.Flags().StringVar(&request.StorageClass, "storage-class", request.StorageClass, "Persistent volume class; borq-local uses the k3s local-path provisioner.")
	return command
}

func finishManagementBootstrap(command *cobra.Command, result management.Result) error {
	if result.Namespace == "" {
		return errors.New("bootstrap completed without its Kubernetes access details")
	}
	_, err := fmt.Fprintf(command.OutOrStdout(), "Borq is running in management cluster %q.\nSupporting services are being reconciled by Borq.\n\nSave your management keys:\nborq management keys%s\n\nThe keys command prints the private recovery file location and the login instructions.\n", result.ClusterName, managementKubeconfigFlag(result.KubeconfigPath))
	return err
}

func quoteLoginArgument(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
