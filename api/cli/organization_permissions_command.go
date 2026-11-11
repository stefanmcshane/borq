package cli

import (
	"github.com/spf13/cobra"
	"github.com/stefanmcshane/borq/api/appapi"
)

func newOrganizationPermissionsCommand() *cobra.Command {
	scope := applicationScope{}
	command := &cobra.Command{Use: "permissions NAME", Short: "Read an organization's administrator permissions.", Args: cobra.ExactArgs(1), RunE: func(command *cobra.Command, arguments []string) error {
		client, err := applicationClient(scope.server)
		if err != nil {
			return err
		}
		response, err := client.GetOrganizationPermissions(command.Context(), arguments[0])
		value, err := readManagedResponse[appapi.OrganizationPermissions](response, err)
		if err != nil {
			return err
		}
		return printApplicationValue(command.OutOrStdout(), value)
	}}
	addApplicationServerFlag(command, &scope)
	return command
}
