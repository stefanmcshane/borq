package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/stefanmcshane/borq/api/appapi"
)

func newManagementSecretsCommand() *cobra.Command {
	command := &cobra.Command{Use: "secrets", Short: "Manage organization secret groups in management OpenBao."}
	for _, verb := range []string{"list", "get", "put"} {
		options := environmentGroupOptions{}
		organizationAlias := ""
		use, arguments := verb+" NAME", cobra.ExactArgs(1)
		if verb == "list" {
			use, arguments = verb, cobra.NoArgs
		}
		child := &cobra.Command{Use: use, Short: fmt.Sprintf("%s management secret groups.", verb), Args: arguments, RunE: func(command *cobra.Command, arguments []string) error {
			if len(arguments) > 0 {
				options.name = arguments[0]
			}
			return executeManagementSecret(command, verb, options)
		}}
		addApplicationServerFlag(child, &options.scope)
		child.Flags().StringVar(&options.scope.organization, "organization", "", "Organization owning these management secret groups.")
		child.Flags().StringVar(&organizationAlias, "org", "", "Alias for --organization.")
		child.PreRunE = func(command *cobra.Command, arguments []string) error {
			if command.Flags().Changed("org") {
				if command.Flags().Changed("organization") && options.scope.organization != organizationAlias {
					return errors.New("--org and --organization must select the same organization")
				}
				if err := command.Flags().Set("organization", organizationAlias); err != nil {
					return err
				}
			}
			return prepareApplicationScope(command, &options.scope)
		}
		if verb == "get" {
			child.Flags().StringVar(&options.version, "version", "latest", "Immutable version to read, or latest.")
		}
		if verb == "put" {
			child.Flags().StringVarP(&options.file, "file", "f", "", "JSON or YAML secret-group document with type, data and optional baseVersion.")
			_ = child.MarkFlagRequired("file")
		}
		command.AddCommand(child)
	}
	return command
}

func executeManagementSecret(command *cobra.Command, operation string, options environmentGroupOptions) error {
	var body appapi.PutSecretInput
	if operation == "put" {
		if err := readManagedDocument(options.file, "PutSecretInput", &body); err != nil {
			return err
		}
		if string(body.Type) != "secret-group" {
			return errors.New("management secrets support only secret-group")
		}
	}
	client, err := applicationClient(options.scope.server)
	if err != nil {
		return err
	}
	ctx, organization := command.Context(), options.scope.organization
	switch operation {
	case "list":
		response, err := client.ListManagementSecretsWithResponse(ctx, organization)
		if err != nil {
			return err
		}
		if response.JSON200 == nil {
			return applicationFailure(response.StatusCode(), response.Body)
		}
		return printApplicationValue(command.OutOrStdout(), response.JSON200)
	case "get":
		response, err := client.GetManagementSecretVersionWithResponse(ctx, organization, options.name, options.version)
		if err != nil {
			return err
		}
		if response.JSON200 == nil {
			return applicationFailure(response.StatusCode(), response.Body)
		}
		return printApplicationValue(command.OutOrStdout(), response.JSON200)
	case "put":
		response, err := client.PutManagementSecretWithResponse(ctx, organization, options.name, body)
		if err != nil {
			return err
		}
		if response.JSON200 == nil {
			return applicationFailure(response.StatusCode(), response.Body)
		}
		// Publishing confirms the selected version without echoing submitted credentials.
		return printApplicationValue(command.OutOrStdout(), map[string]any{"name": response.JSON200.Name, "type": response.JSON200.Type, "version": response.JSON200.Version})
	}
	return nil
}
