package cli

import (
	"time"

	"github.com/spf13/cobra"
	"github.com/stefanmcshane/borq/api/appserver"
	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/logger"
)

func newApplicationServerCommand(log logger.Logger, configuration Config) *cobra.Command {
	networkFile := configuration.NetworkConfigFile
	input := appserver.NewInput{Version: configuration.Version, Port: configuration.Port, TelemetryPort: configuration.TelemetryPort, DataDir: configuration.DataDir,
		ManagementEnabled:  configuration.ManagementEnabled,
		PrometheusQueryUrl: configuration.PrometheusQueryUrl, LokiQueryUrl: configuration.LokiQueryUrl,
		InfrastructureEnabled: configuration.InfrastructureEnabled, ManagementKubeconfigPath: configuration.ManagementKubeconfigPath,
		PlatformSettings: platform.Settings{ApiUrl: configuration.ApiUrl, ClusterName: configuration.ManagementClusterName, Profile: configuration.InfrastructureProfile, Namespace: platform.ManagementNamespace, OpenBaoURL: configuration.ManagementOpenBaoURL, TelemetryURL: configuration.ManagementTelemetryURL, LokiURL: configuration.ManagementLokiURL, PrometheusURL: configuration.ManagementPrometheusURL, S3Endpoint: configuration.ManagementS3Endpoint, StorageClass: configuration.InfrastructureStorageClass},
	}
	command := &cobra.Command{
		Use: "server", Short: "Run the application API with persistent SQLite state.", Args: cobra.NoArgs, Version: configuration.Version,
		RunE: func(command *cobra.Command, arguments []string) error {
			if networkFile != "" {
				settings, err := readNetworkSettings(networkFile, input.PlatformSettings.ClusterName)
				if err != nil {
					return err
				}
				input.PlatformSettings.Network = settings
			}
			server, err := appserver.New(command.Context(), input)
			if err != nil {
				return err
			}
			log.Info("starting_application_server", logger.WithFields(logger.String("version", configuration.Version)))
			return server.Serve(command.Context())
		},
	}
	command.Flags().StringVar(&input.AdminTokenFile, "admin-token-file", "", "Read the server administrator token from a private file; otherwise keep one in the data directory.")
	command.Flags().IntVar(&input.Port, "port", input.Port, "HTTP listen port.")
	command.Flags().StringVar(&networkFile, "network-config", networkFile, "Bootstrap network settings file; credentials remain in management Secrets.")
	command.Flags().IntVar(&input.TelemetryPort, "telemetry-port", input.TelemetryPort, "Public authenticated ingestion port; administrative routes are excluded.")
	command.Flags().StringVar(&input.PrometheusQueryUrl, "prometheus-query-url", input.PrometheusQueryUrl, "Private Prometheus query URL; empty uses management service discovery.")
	command.Flags().StringVar(&input.LokiQueryUrl, "loki-query-url", input.LokiQueryUrl, "Private Loki query URL; empty uses management service discovery.")
	command.Flags().StringVar(&input.ListenHost, "listen", "127.0.0.1", "HTTP listen interface.")
	command.Flags().StringVar(&input.DataDir, "data-dir", input.DataDir, "Persistent application state directory.")
	command.Flags().StringVar(&input.PlatformSettings.ApiUrl, "api-url", input.PlatformSettings.ApiUrl, "Advertised management API address; component endpoints default automatically.")
	command.Flags().DurationVar(&input.RolloutTimeout, "rollout-timeout", 10*time.Minute, "Maximum server-side rollout duration before reporting failure.")
	command.Flags().BoolVar(&input.InfrastructureEnabled, "infrastructure-enabled", input.InfrastructureEnabled, "Manage platform components when clusters are registered.")
	command.Flags().BoolVar(&input.ManagementEnabled, "management-enabled", input.ManagementEnabled, "Install management components in the background after this server starts.")
	command.Flags().BoolVar(&input.ManagementBootstrapOnly, "management-bootstrap-only", false, "Allow initial management configuration before public networking is configured.")
	command.Flags().StringVar(&input.PlatformSettings.ClusterName, "management-cluster-name", input.PlatformSettings.ClusterName, "Management installation name used to name shared storage.")
	command.Flags().StringVar(&input.ManagementKubeconfigPath, "management-kubeconfig", input.ManagementKubeconfigPath, "Management kubeconfig; empty uses the pod service account.")
	command.Flags().StringVar(&input.PlatformSettings.Profile, "infrastructure-profile", input.PlatformSettings.Profile, "Component resource profile: dev or prod.")
	command.Flags().StringVar(&input.PlatformSettings.OpenBaoURL, "management-openbao-url", input.PlatformSettings.OpenBaoURL, "Management vault URL reachable from target clusters.")
	command.Flags().StringVar(&input.PlatformSettings.TelemetryURL, "management-telemetry-url", input.PlatformSettings.TelemetryURL, "Authenticated Borq telemetry gateway URL reachable from targets.")
	command.Flags().StringVar(&input.PlatformSettings.LokiURL, "management-loki-url", input.PlatformSettings.LokiURL, "Optional authenticated Loki push URL override.")
	command.Flags().StringVar(&input.PlatformSettings.PrometheusURL, "management-prometheus-url", input.PlatformSettings.PrometheusURL, "Optional authenticated Prometheus remote-write URL override.")
	command.Flags().StringVar(&input.PlatformSettings.S3Endpoint, "management-s3-endpoint", input.PlatformSettings.S3Endpoint, "Built-in S3 endpoint reachable from deployment targets.")
	command.Flags().StringVar(&input.PlatformSettings.StorageClass, "infrastructure-storage-class", input.PlatformSettings.StorageClass, "Storage class used by management components.")
	for _, name := range []string{"management-s3-endpoint", "infrastructure-profile", "management-openbao-url", "management-telemetry-url", "management-loki-url", "management-prometheus-url"} {
		_ = command.Flags().MarkHidden(name)
	}
	return command
}
