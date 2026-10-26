package cli

import (
	"time"

	"github.com/spf13/cobra"
	"github.com/stefanmcshane/borq/api/appserver"
	"github.com/stefanmcshane/borq/business/server/platform"
)

func newApplicationServerCommand(configuration Config) *cobra.Command {
	input := appserver.NewInput{Port: configuration.Port, TelemetryPort: configuration.TelemetryPort, DataDir: configuration.DataDir,
		ManagementEnabled:  configuration.ManagementEnabled,
		PrometheusQueryUrl: configuration.PrometheusQueryUrl, LokiQueryUrl: configuration.LokiQueryUrl,
		InfrastructureEnabled: configuration.InfrastructureEnabled, ManagementKubeconfigPath: configuration.ManagementKubeconfigPath,
		PlatformSettings: platform.Settings{ApiUrl: configuration.ApiUrl, Profile: configuration.InfrastructureProfile, Namespace: "borq", OpenBaoURL: configuration.ManagementOpenBaoURL, TelemetryURL: configuration.ManagementTelemetryURL, LokiURL: configuration.ManagementLokiURL, PrometheusURL: configuration.ManagementPrometheusURL, S3Endpoint: configuration.ManagementS3Endpoint, StorageClass: configuration.InfrastructureStorageClass},
	}
	command := &cobra.Command{
		Use: "server", Short: "Run the application API with persistent SQLite state.", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, arguments []string) error {
			server, err := appserver.New(command.Context(), input)
			if err != nil {
				return err
			}
			return server.Serve(command.Context())
		},
	}
	command.Flags().IntVar(&input.Port, "port", input.Port, "HTTP listen port.")
	command.Flags().IntVar(&input.TelemetryPort, "telemetry-port", input.TelemetryPort, "Public authenticated ingestion port; administrative routes are excluded.")
	command.Flags().StringVar(&input.PrometheusQueryUrl, "prometheus-query-url", input.PrometheusQueryUrl, "Private Prometheus query URL; empty uses management service discovery.")
	command.Flags().StringVar(&input.LokiQueryUrl, "loki-query-url", input.LokiQueryUrl, "Private Loki query URL; empty uses management service discovery.")
	command.Flags().StringVar(&input.ListenHost, "listen", "127.0.0.1", "HTTP listen interface.")
	command.Flags().StringVar(&input.DataDir, "data-dir", input.DataDir, "Persistent application state directory.")
	command.Flags().StringVar(&input.PlatformSettings.ApiUrl, "api-url", input.PlatformSettings.ApiUrl, "Advertised management API address; component endpoints default automatically.")
	command.Flags().DurationVar(&input.RolloutTimeout, "rollout-timeout", 10*time.Minute, "Maximum server-side rollout duration before reporting failure.")
	command.Flags().BoolVar(&input.InfrastructureEnabled, "infrastructure-enabled", input.InfrastructureEnabled, "Manage platform components when clusters are registered.")
	command.Flags().BoolVar(&input.ManagementEnabled, "management-enabled", input.ManagementEnabled, "Install management components after this server starts; readiness waits for completion.")
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
