package cli

import (
	"errors"
	"net/http"

	"github.com/spf13/cobra"
	"github.com/stefanmcshane/borq/api/appapi"
)

func newOrganizationRolesCommand() *cobra.Command {
	group := &cobra.Command{Use: "roles", Short: "Manage reusable organization roles."}
	listScope := applicationScope{}
	list := &cobra.Command{Use: "list ORGANIZATION", Short: "List organization roles and their permissions.", Args: cobra.ExactArgs(1), RunE: func(command *cobra.Command, arguments []string) error {
		return executeOrganizationRolesList(command, listScope, arguments[0])
	}}
	addApplicationServerFlag(list, &listScope)
	createScope, createFile := applicationScope{}, ""
	create := &cobra.Command{Use: "create ORGANIZATION", Short: "Create a role from a YAML file with revision zero.", Args: cobra.ExactArgs(1), RunE: func(command *cobra.Command, arguments []string) error {
		return executeOrganizationRolesWrite(command, createScope, arguments[0], "", createFile)
	}}
	addApplicationServerFlag(create, &createScope)
	create.Flags().StringVarP(&createFile, "file", "f", "", "YAML containing name, permissions and revision zero. Use --template to generate it.")
	_ = create.MarkFlagRequired("file")
	updateScope, updateFile := applicationScope{}, ""
	update := &cobra.Command{Use: "update ORGANIZATION ROLE", Short: "Replace a role using its current revision.", Args: cobra.ExactArgs(2), RunE: func(command *cobra.Command, arguments []string) error {
		return executeOrganizationRolesWrite(command, updateScope, arguments[0], arguments[1], updateFile)
	}}
	addApplicationServerFlag(update, &updateScope)
	update.Flags().StringVarP(&updateFile, "file", "f", "", "YAML containing name, permissions and the last read revision. Use --template to generate it.")
	_ = update.MarkFlagRequired("file")
	deleteScope, deleteRevision := applicationScope{}, int64(0)
	remove := &cobra.Command{Use: "delete ORGANIZATION ROLE", Short: "Delete an unassigned role using its current revision.", Args: cobra.ExactArgs(2), RunE: func(command *cobra.Command, arguments []string) error {
		return executeOrganizationRolesDelete(command, deleteScope, arguments[0], arguments[1], deleteRevision)
	}}
	addApplicationServerFlag(remove, &deleteScope)
	remove.Flags().Int64Var(&deleteRevision, "revision", 0, "Last read role revision.")
	_ = remove.MarkFlagRequired("revision")
	group.AddCommand(list, create, update, remove)
	return group
}

func executeOrganizationRolesList(command *cobra.Command, scope applicationScope, organization string) error {
	client, err := applicationClient(scope.server)
	if err != nil {
		return err
	}
	response, err := client.ListOrganizationRoles(command.Context(), organization)
	value, err := readManagedResponse[[]appapi.OrganizationRole](response, err)
	if err != nil {
		return err
	}
	return printApplicationValue(command.OutOrStdout(), value)
}

func executeOrganizationRolesWrite(command *cobra.Command, scope applicationScope, organization, role, file string) error {
	var input appapi.OrganizationRoleInput
	if err := readManagedDocument(file, "OrganizationRoleInput", &input); err != nil {
		return err
	}
	client, err := applicationClient(scope.server)
	if err != nil {
		return err
	}
	var response *http.Response
	if role == "" {
		response, err = client.CreateOrganizationRole(command.Context(), organization, input)
	} else {
		response, err = client.PutOrganizationRole(command.Context(), organization, role, input)
	}
	value, err := readManagedResponse[appapi.OrganizationRole](response, err)
	if err != nil {
		return err
	}
	return printApplicationValue(command.OutOrStdout(), value)
}

func executeOrganizationRolesDelete(command *cobra.Command, scope applicationScope, organization, role string, revision int64) error {
	if revision < 1 {
		return errors.New("revision must be positive")
	}
	client, err := applicationClient(scope.server)
	if err != nil {
		return err
	}
	response, err := client.DeleteOrganizationRole(command.Context(), organization, role, &appapi.DeleteOrganizationRoleParams{Revision: revision})
	value, err := readManagedResponse[appapi.DeletedOrganizationRole](response, err)
	if err != nil {
		return err
	}
	return printApplicationValue(command.OutOrStdout(), value)
}
