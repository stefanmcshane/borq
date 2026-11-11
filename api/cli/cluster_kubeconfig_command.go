package cli

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"k8s.io/client-go/tools/clientcmd"
)

type clusterKubeconfigOptions struct {
	scope             applicationScope
	output, apiServer string
}

func newClusterKubeconfigCommand() *cobra.Command {
	options := clusterKubeconfigOptions{}
	kubeconfig := &cobra.Command{Use: "kubeconfig [NAME]", Short: "Download a linked cluster's kubeconfig from the management server.", Args: cobra.MaximumNArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			name, err := clusterArgument(arguments, options.scope.cluster)
			if err != nil {
				return err
			}
			return executeClusterKubeconfig(command, options, name)
		},
	}
	addOrganizationScopeFlags(kubeconfig, &options.scope)
	kubeconfig.Flags().StringVarP(&options.output, "output", "o", "", "Write a private kubeconfig file instead of stdout; - selects stdout.")
	kubeconfig.Flags().StringVar(&options.apiServer, "api-server", "", "Override the Kubernetes API URL in the downloaded kubeconfig.")
	return kubeconfig
}

// Cobra owns the command pointer and its context and output streams.
func executeClusterKubeconfig(command *cobra.Command, options clusterKubeconfigOptions, name string) error {
	if options.scope.organization == "" {
		return errors.New("provide --organization or set a default with borq config set --organization NAME")
	}
	client, err := applicationClient(options.scope.server)
	if err != nil {
		return err
	}
	response, err := client.GetClusterKubeconfigWithResponse(command.Context(), options.scope.organization, name)
	if err != nil {
		return fmt.Errorf("download cluster kubeconfig: %w", err)
	}
	if response.JSON200 == nil {
		if response.StatusCode() == http.StatusNotFound {
			return fmt.Errorf("cluster %q was not found in organization %q", name, options.scope.organization)
		}
		return fmt.Errorf("download cluster kubeconfig: management API returned status %d", response.StatusCode())
	}
	source := response.JSON200.Kubeconfig
	configuration, err := clientcmd.Load([]byte(source))
	if err != nil || clientcmd.Validate(*configuration) != nil {
		return errors.New("management API returned an invalid cluster kubeconfig")
	}
	if options.apiServer != "" {
		if err := selectClusterLinkEndpoint(configuration, "", options.apiServer); err != nil {
			return err
		}
		encoded, err := clientcmd.Write(*configuration)
		if err != nil {
			return errors.New("could not encode downloaded cluster kubeconfig")
		}
		source = string(encoded)
	}
	if options.output == "" || options.output == "-" {
		_, err := io.WriteString(command.OutOrStdout(), source)
		return err
	}
	if err := writeClusterKubeconfig(options.output, source); err != nil {
		return err
	}
	_, err = fmt.Fprintf(command.ErrOrStderr(), "Kubeconfig saved to %s. Select this file with kubectl --kubeconfig or KUBECONFIG.\n", options.output)
	return err
}

func writeClusterKubeconfig(path, source string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create kubeconfig output directory: %w", err)
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".borq-kubeconfig-*")
	if err != nil {
		return fmt.Errorf("create kubeconfig output: %w", err)
	}
	defer os.Remove(file.Name())
	if _, err := io.WriteString(file, source); err != nil {
		_ = file.Close()
		return fmt.Errorf("write kubeconfig output: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close kubeconfig output: %w", err)
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return fmt.Errorf("save kubeconfig output: %w", err)
	}
	return nil
}
