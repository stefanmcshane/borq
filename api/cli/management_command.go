package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

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

	command.AddCommand(newManagementBootstrapCommand(ctx, log, cfg))
	command.AddCommand(newManagementApplyCommand(), newManagementExportCommand(), newManagementBackupsCommand())
	command.AddCommand(newManagementServerCommand(log, cfg))

	return command
}

func newManagementBootstrapCommand(ctx context.Context, log logger.Logger, cfg Config) *cobra.Command {
	request := management.DefaultRequest()
	request.KubeconfigPath = cfg.KubeconfigPath
	defaultImage := request.BorqImage
	// Leave image selection unset so a bundled image is used only when no image was requested.
	request.BorqImage = ""
	request.ProbeURL = ""
	request.Mode = ""
	development := false

	command := &cobra.Command{
		Use:   "bootstrap [CLUSTER]",
		Short: "Install the Borq server on an existing management Kubernetes cluster.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			settings, err := selectedClientSettings(cmd)
			if err != nil {
				return err
			}
			if len(args) == 0 && !cmd.Flags().Changed("cluster-name") && settings.Cluster != "" {
				request.ClusterName = settings.Cluster
			}
			if len(args) == 1 {
				if cmd.Flags().Changed("cluster-name") && request.ClusterName != args[0] {
					return fmt.Errorf("cluster argument and --cluster-name differ; provide one cluster name")
				}
				request.ClusterName = args[0]
			}
			if !cmd.Flags().Changed("api-url") && settings.CurrentProfile != "" {
				request.ApiUrl = settings.Server
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

			result, err := management.Execute(executionContext, log, request)
			if err != nil {
				return err
			}

			_, err = fmt.Fprintf(
				cmd.OutOrStdout(),
				"management cluster %q is reachable at %s\nkubeconfig: %s\n",
				result.ClusterName,
				result.ApiUrl,
				result.KubeconfigPath,
			)
			return err
		},
	}

	command.Flags().StringVar(&request.ClusterName, "cluster-name", request.ClusterName, "Management installation name.")
	command.Flags().StringVar(&request.ApiUrl, "api-url", "", "Borq API base URL; defaults to http://localhost:7833 for local Docker or http://localhost:31833 for native/external clusters.")
	// Keep the request unset so profile and custom port defaults remain business-owned.
	command.Flags().Lookup("api-url").DefValue = "http://localhost:31833"
	command.Flags().Int32Var(&request.HostPort, "host-port", request.HostPort, "Local Docker API port; bootstrap publishes it automatically.")
	command.Flags().Int32Var(&request.NodePort, "node-port", request.NodePort, "Management Borq service NodePort.")
	command.Flags().Int32Var(&request.TelemetryHostPort, "telemetry-host-port", request.TelemetryHostPort, "Already-published development ingestion port used for endpoint defaults.")
	command.Flags().Int32Var(&request.TelemetryNodePort, "telemetry-node-port", request.TelemetryNodePort, "Public ingestion service NodePort.")
	command.Flags().DurationVar(&request.Timeout, "timeout", request.Timeout, "Overall bootstrap timeout.")
	command.Flags().StringVar(&request.Namespace, "namespace", request.Namespace, "Management namespace (must be borq).")
	command.Flags().StringVar(&request.ProbeURL, "probe-url", request.ProbeURL, "Advanced health endpoint override; normally derived from --api-url.")
	command.Flags().StringVar(&request.KubeconfigPath, "kubeconfig", request.KubeconfigPath, "Existing cluster kubeconfig; uses configured credentials or the local k3s kubeconfig when omitted.")
	command.Flags().StringVar(&request.BorqImage, "server-image", "", "Borq server image reference or tag; an explicit image overrides automatic archive discovery.")
	command.Flags().Lookup("server-image").DefValue = defaultImage
	command.Flags().StringVar(&request.ServerImageArchivePath, "server-image-archive", "", "Offline server image archive for local k3d; the bundled archive is detected automatically.")
	command.Flags().StringVar(&request.BorqImage, "borq-image", "", "Alias for --server-image.")
	command.Flags().Lookup("borq-image").DefValue = defaultImage

	command.Flags().BoolVar(&development, "development", false, "Use persistent single-node Raft and lightweight component defaults; inferred for local Docker clusters.")
	command.Flags().Int32Var(&request.OpenbaoHostPort, "openbao-host-port", request.OpenbaoHostPort, "Already-published development vault port used for endpoint defaults.")
	command.Flags().StringVar(&request.OpenbaoUrl, "management-openbao-url", request.OpenbaoUrl, "Advanced override for the management OpenBao URL reachable from deployment targets.")
	command.Flags().StringVar(&request.TelemetryUrl, "management-telemetry-url", request.TelemetryUrl, "Advanced override for the Borq telemetry URL reachable from deployment targets.")
	command.Flags().StringVar(&request.LokiUrl, "management-loki-url", request.LokiUrl, "Advanced override for the full authenticated Loki push endpoint.")
	command.Flags().StringVar(&request.PrometheusUrl, "management-prometheus-url", request.PrometheusUrl, "Advanced override for the full authenticated Prometheus remote-write endpoint.")
	command.Flags().StringVar(&request.S3Endpoint, "management-s3-endpoint", request.S3Endpoint, "Built-in S3 endpoint reachable from deployment targets.")
	command.Flags().StringVar(&request.StorageClass, "storage-class", request.StorageClass, "Persistent volume class; borq-local uses the k3s local-path provisioner.")
	for _, name := range []string{"management-s3-endpoint", "probe-url", "management-openbao-url", "management-telemetry-url", "management-loki-url", "management-prometheus-url"} {
		_ = command.Flags().MarkHidden(name)
	}
	return command
}
