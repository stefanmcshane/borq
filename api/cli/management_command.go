package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/stefanmcshane/borq/business/cli/management"
	"github.com/stefanmcshane/borq/foundations/logger"
)

func newManagementCommand(ctx context.Context, log logger.Logger, cfg Config) *cobra.Command {
	command := &cobra.Command{
		Use:   "management",
		Short: "Operate the borq management cluster.",
	}

	command.AddCommand(newManagementBootstrapCommand(ctx, log))
	command.AddCommand(newManagementServerCommand(log, cfg))

	return command
}

func newManagementBootstrapCommand(ctx context.Context, log logger.Logger) *cobra.Command {
	request := management.DefaultRequest()
	request.ProbeURL = ""

	kubeconfigPaths := filepath.SplitList(os.Getenv("KUBECONFIG"))
	for _, p := range kubeconfigPaths {
		if strings.TrimSpace(p) != "" {
			request.KubeconfigPath = p
			break
		}
	}
	if request.KubeconfigPath == "" {
		home, _ := os.UserHomeDir()
		request.KubeconfigPath = filepath.Join(home, ".kube", "config")
	}

	command := &cobra.Command{
		Use:   "bootstrap",
		Short: "Bootstrap the local borq management cluster.",
		RunE: func(cmd *cobra.Command, args []string) error {
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
				"management cluster %q is reachable at %s\n",
				result.ClusterName,
				result.ProbeURL,
			)
			return err
		},
	}

	command.Flags().StringVar(&request.ClusterName, "cluster-name", request.ClusterName, "Management kind cluster name.")
	command.Flags().Int32Var(&request.HostPort, "host-port", request.HostPort, "Host port to map to the borq management service.")
	command.Flags().DurationVar(&request.Timeout, "timeout", request.Timeout, "Overall bootstrap timeout.")
	command.Flags().StringVar(&request.Namespace, "namespace", request.Namespace, "Namespace for borq management bootstrap resources.")
	command.Flags().StringVar(&request.ProbeURL, "probe-url", request.ProbeURL, "Optional management health endpoint URL override.")
	command.Flags().StringVar(&request.BorqImage, "server-image", request.BorqImage, "Borq server image to deploy.")
	command.Flags().StringVar(&request.BorqImage, "borq-image", request.BorqImage, "Borq server image to deploy.")

	command.Flags().Lookup("timeout").DefValue = (10 * time.Minute).String()

	return command
}
