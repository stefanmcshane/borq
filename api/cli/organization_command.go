package cli

import "github.com/spf13/cobra"

func newOrganizationCommand() *cobra.Command {
	group := &cobra.Command{Use: "org", Short: "Manage organizations."}
	createScope := applicationScope{}
	create := &cobra.Command{Use: "create NAME", Short: "Create an organization.", Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			return executeOrganizationCreate(command, createScope, arguments[0])
		},
	}
	addApplicationServerFlag(create, &createScope)
	listScope := applicationScope{}
	list := &cobra.Command{Use: "list", Short: "List organizations.", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, arguments []string) error {
			return executeOrganizationList(command, listScope)
		},
	}
	addApplicationServerFlag(list, &listScope)
	group.AddCommand(create, list)
	return group
}

func executeOrganizationCreate(command *cobra.Command, scope applicationScope, name string) error {
	client, err := applicationClient(scope.server)
	if err != nil {
		return err
	}
	response, err := client.PutOrganizationWithResponse(command.Context(), name)
	if err != nil {
		return err
	}
	if response.JSON200 == nil {
		return applicationFailure(response.StatusCode(), response.Body)
	}
	return printApplicationValue(command.OutOrStdout(), response.JSON200)
}

func executeOrganizationList(command *cobra.Command, scope applicationScope) error {
	client, err := applicationClient(scope.server)
	if err != nil {
		return err
	}
	response, err := client.ListOrganizationsWithResponse(command.Context())
	if err != nil {
		return err
	}
	if response.JSON200 == nil {
		return applicationFailure(response.StatusCode(), response.Body)
	}
	return printApplicationValue(command.OutOrStdout(), response.JSON200)
}
