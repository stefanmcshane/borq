package cli

import (
	"github.com/spf13/cobra"
	"github.com/stefanmcshane/borq/api/server"
	"github.com/stefanmcshane/borq/foundations/logger"
)

const defaultServerDataDir = "/var/lib/borq"

func newManagementServerCommand(log logger.Logger) *cobra.Command {
	config := server.Config{
		Port:    8080,
		DataDir: defaultServerDataDir,
	}

	command := &cobra.Command{
		Use:   "server",
		Short: "Run the borq management server.",
		RunE: func(cmd *cobra.Command, args []string) error {
			runtimeServer, err := server.NewServer(config, log)
			if err != nil {
				return err
			}

			return runtimeServer.Serve(cmd.Context())
		},
	}

	command.Flags().IntVar(&config.Port, "port", config.Port, "HTTP listen port.")
	command.Flags().StringVar(&config.DataDir, "data-dir", config.DataDir, "Persistent data directory.")

	return command
}
