package cli

import "github.com/spf13/cobra"

type appsListOptions struct {
	scope    applicationScope
	manifest bool
}

func newAppsCommand() *cobra.Command {
	group := &cobra.Command{Use: "apps", Short: "Inspect applications and manage their deployments."}
	options := appsListOptions{}
	list := &cobra.Command{Use: "list", Short: "Print the space's expanded app YAML or saved Kubernetes manifests.", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, arguments []string) error {
			return executeAppsList(command, options)
		},
	}
	addApplicationServerFlag(list, &options.scope)
	list.Flags().StringVar(&options.scope.organization, "organization", "", "Organization owning the space.")
	list.Flags().StringVar(&options.scope.space, "space", "", "Space whose apps to read.")
	list.Flags().BoolVar(&options.manifest, "manifest", false, "Print the exact Kubernetes manifests saved by the latest accepted apply.")
	group.AddCommand(list, newAppsHistoryCommand(), newAppsCancelCommand())
	return group
}

func executeAppsList(command *cobra.Command, options appsListOptions) error {
	client, err := applicationClient(options.scope.server)
	if err != nil {
		return err
	}
	response, err := client.ListAppsWithResponse(command.Context(), options.scope.organization, options.scope.space)
	if err != nil {
		return err
	}
	if response.JSON200 == nil {
		return applicationFailure(response.StatusCode(), response.Body)
	}
	if options.manifest {
		return printApplicationManifests(command.OutOrStdout(), response.JSON200.Manifests)
	}
	return printApplicationValue(command.OutOrStdout(), response.JSON200.Configuration)
}
