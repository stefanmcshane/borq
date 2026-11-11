package cli

import (
	"github.com/spf13/cobra"
	"github.com/stefanmcshane/borq/api/appapi"
)

func newOrganizationMembersCommand() *cobra.Command {
	group := &cobra.Command{Use: "members", Short: "Manage provider-approved members and their assigned roles."}
	listScope := applicationScope{}
	list := &cobra.Command{Use: "list ORGANIZATION", Short: "List active and disabled members.", Args: cobra.ExactArgs(1), RunE: func(command *cobra.Command, arguments []string) error {
		return executeOrganizationMembersList(command, listScope, arguments[0])
	}}
	addApplicationServerFlag(list, &listScope)
	updateScope, updateFile := applicationScope{}, ""
	update := &cobra.Command{Use: "update ORGANIZATION MEMBER", Short: "Replace a member's assigned roles or disable access using its current revision.", Args: cobra.ExactArgs(2), RunE: func(command *cobra.Command, arguments []string) error {
		return executeOrganizationMembersUpdate(command, updateScope, arguments[0], arguments[1], updateFile)
	}}
	addApplicationServerFlag(update, &updateScope)
	update.Flags().StringVarP(&updateFile, "file", "f", "", "YAML containing roleIds, disabled and the last read revision. Use --template to generate it.")
	_ = update.MarkFlagRequired("file")
	group.AddCommand(list, update)
	return group
}

func executeOrganizationMembersList(command *cobra.Command, scope applicationScope, organization string) error {
	client, err := applicationClient(scope.server)
	if err != nil {
		return err
	}
	response, err := client.ListOrganizationMembers(command.Context(), organization)
	value, err := readManagedResponse[[]appapi.OrganizationMember](response, err)
	if err != nil {
		return err
	}
	return printApplicationValue(command.OutOrStdout(), value)
}

func executeOrganizationMembersUpdate(command *cobra.Command, scope applicationScope, organization string, member string, file string) error {
	var input appapi.OrganizationMemberInput
	if err := readManagedDocument(file, "OrganizationMemberInput", &input); err != nil {
		return err
	}
	client, err := applicationClient(scope.server)
	if err != nil {
		return err
	}
	response, err := client.PutOrganizationMember(command.Context(), organization, member, input)
	value, err := readManagedResponse[appapi.OrganizationMember](response, err)
	if err != nil {
		return err
	}
	return printApplicationValue(command.OutOrStdout(), value)
}
