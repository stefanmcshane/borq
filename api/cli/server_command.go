package cli

import (
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/stefanmcshane/borq/api/server"
	"github.com/stefanmcshane/borq/foundations/logger"
)

const (
	defaultServerDataDir = "/var/lib/borq"
	kubeconfigEnvVar     = "BORQ_KUBECONFIG"
)

func newManagementServerCommand(log logger.Logger) *cobra.Command {
	input := server.NewServerInput{
		Port:           7833,
		DataDir:        defaultServerDataDir,
		KubeconfigPath: strings.TrimSpace(os.Getenv(kubeconfigEnvVar)),
		Log:            log,
	}

	command := &cobra.Command{
		Use:   "server",
		Short: "Run the borq management server.",
		RunE: func(cmd *cobra.Command, args []string) error {
			runtimeServer, err := server.NewServer(cmd.Context(), input)
			if err != nil {
				return err
			}

			return runtimeServer.Serve(cmd.Context())
		},
	}

	command.Flags().IntVar(&input.Port, "port", input.Port, "HTTP listen port.")
	command.Flags().StringVar(&input.DataDir, "data-dir", input.DataDir, "Persistent data directory.")
	command.Flags().StringVar(&input.KubeconfigPath, "kubeconfig", input.KubeconfigPath, "Optional kubeconfig file path for the managed cluster.")

	return command
}
