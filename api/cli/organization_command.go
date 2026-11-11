package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/stefanmcshane/borq/api/appapi"
	"sigs.k8s.io/yaml"
)

type organizationCreateOptions struct {
	file                 string
	pocketIdOrganization string
	adminEmail           string
}

func newOrganizationCommand() *cobra.Command {
	group := &cobra.Command{Use: "org", Short: "Manage organizations."}
	createScope := applicationScope{}
	options := organizationCreateOptions{}
	create := &cobra.Command{Use: "create NAME", Short: "Create an organization with optional OIDC sign-in.", Args: cobra.ExactArgs(1),
		Long: "Create an organization using administrator-token access until OIDC is configured. To supply external OIDC now, run borq org create --template=oidc > organization.yaml, fill in the provider settings and administrator, then run borq org create NAME --file organization.yaml. Pocket ID can be deployed separately as a managed application. Use --template=pocket-id to explicitly provision an organization-owned instance, or --pocketid-organization NAME to reuse one.",
		RunE: func(command *cobra.Command, arguments []string) error {
			return executeOrganizationCreate(command, createScope, arguments[0], options)
		},
	}
	create.Flags().StringVarP(&options.file, "file", "f", "", "Organization login configuration YAML (adminEmail and oidc, or pocketId).")
	create.Flags().StringVar(&options.pocketIdOrganization, "pocketid-organization", "", "Organization owning the existing managed Pocket ID instance to reuse.")
	create.Flags().StringVar(&options.adminEmail, "admin-email", "", "Verified provider account allowed to administer this Borq organization; grants no Pocket ID administration.")
	addApplicationServerFlag(create, &createScope)
	listScope := applicationScope{}
	list := &cobra.Command{Use: "list", Short: "List organizations.", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, arguments []string) error {
			return executeOrganizationList(command, listScope)
		},
	}
	addApplicationServerFlag(list, &listScope)
	group.AddCommand(create, list, newOrganizationPermissionsCommand(), newOrganizationMembersCommand(), newOrganizationRolesCommand(), newOrganizationOidcCommand())
	return group
}

func executeOrganizationCreate(command *cobra.Command, scope applicationScope, name string, options organizationCreateOptions) error {
	input := appapi.OrganizationOnboardingInput{}
	if options.file != "" {
		source, err := os.ReadFile(options.file)
		if err != nil {
			return fmt.Errorf("read organization login configuration: %w", err)
		}
		if err := yaml.UnmarshalStrict(source, &input); err != nil {
			return fmt.Errorf("parse organization login configuration: %w", err)
		}
	}
	if options.pocketIdOrganization != "" {
		input.PocketIDOrganization = &options.pocketIdOrganization
	}
	if command.Flags().Changed("admin-email") {
		input.AdminEmail = &options.adminEmail
	}
	client, err := applicationClient(scope.server)
	if err != nil {
		return err
	}
	response, err := client.PutOrganizationWithResponse(command.Context(), name, input)
	if err != nil {
		return err
	}
	if response.JSON200 == nil {
		return applicationFailure(response.StatusCode(), response.Body)
	}
	if err := printApplicationValue(command.OutOrStdout(), response.JSON200); err != nil {
		return err
	}
	return nil
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
