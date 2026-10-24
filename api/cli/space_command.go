package cli

import (
	"errors"
	"strings"

	"github.com/spf13/cobra"
	"github.com/stefanmcshane/borq/api/appapi"
)

type spaceCreateOptions struct {
	scope   applicationScope
	cluster string
}

func newSpaceCommand() *cobra.Command {
	group := &cobra.Command{Use: "space", Short: "Manage deployment spaces within an organization."}
	options := spaceCreateOptions{}
	create := &cobra.Command{Use: "create NAME", Short: "Create a space bound to one cluster.", Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			if !command.Flags().Changed("cluster") {
				options.cluster = options.scope.cluster
			}
			return executeSpaceCreate(command, options, arguments[0])
		},
	}
	addOrganizationScopeFlags(create, &options.scope)
	create.Flags().StringVar(&options.cluster, "cluster", "", "Cluster to bind; required unless selected in the current profile or config.")
	listScope := applicationScope{}
	list := &cobra.Command{Use: "list", Short: "List spaces in an organization.", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, arguments []string) error {
			return executeSpaceList(command, listScope)
		},
	}
	addOrganizationScopeFlags(list, &listScope)
	group.AddCommand(create, list)
	return group
}

func executeSpaceCreate(command *cobra.Command, options spaceCreateOptions, name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("provide a space name: borq space create NAME")
	}
	if strings.TrimSpace(options.cluster) == "" {
		return errors.New("provide --cluster or set a default with borq config set --cluster NAME")
	}
	client, err := applicationClient(options.scope.server)
	if err != nil {
		return err
	}
	body := appapi.SpaceInput{Cluster: options.cluster}
	response, err := client.PutSpaceWithResponse(command.Context(), options.scope.organization, name, body)
	if err != nil {
		return err
	}
	if response.JSON200 == nil {
		return applicationFailure(response.StatusCode(), response.Body)
	}
	return printApplicationValue(command.OutOrStdout(), response.JSON200)
}

func executeSpaceList(command *cobra.Command, scope applicationScope) error {
	client, err := applicationClient(scope.server)
	if err != nil {
		return err
	}
	response, err := client.ListSpacesWithResponse(command.Context(), scope.organization)
	if err != nil {
		return err
	}
	if response.JSON200 == nil {
		return applicationFailure(response.StatusCode(), response.Body)
	}
	return printApplicationValue(command.OutOrStdout(), response.JSON200)
}
