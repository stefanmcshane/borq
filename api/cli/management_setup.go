package cli

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/stefanmcshane/borq/api/appapi"
)

// The explicit authenticated apply is the only CLI action that mints initial enrollment links.
func printManagementSetup(command *cobra.Command, client *appapi.ClientWithResponses) error {
	ctx, cancel := context.WithTimeout(command.Context(), 5*time.Second)
	defer cancel()
	response, err := client.GetManagementConfiguration(ctx)
	configuration, err := readManagedResponse[appapi.ManagementConfiguration](response, err)
	if err != nil {
		_, writeErr := fmt.Fprintln(command.ErrOrStderr(), "Warning: management configuration applied, but saved sign-in details could not be read. Run borq management export and borq org oidc get NAME to retrieve them.")
		return writeErr
	}
	if configuration.BorqPublicURL != nil && *configuration.BorqPublicURL != "" {
		if _, err := fmt.Fprintf(command.OutOrStdout(), "Public sign-in: %s/login\n", strings.TrimRight(*configuration.BorqPublicURL, "/")); err != nil {
			return err
		}
	}
	if configuration.Network != nil && configuration.Network.ManagementURL != nil && *configuration.Network.ManagementURL != "" {
		if _, err := fmt.Fprintf(command.OutOrStdout(), "Private sign-in: %s/login\n", strings.TrimRight(*configuration.Network.ManagementURL, "/")); err != nil {
			return err
		}
	}
	if configuration.Organizations == nil {
		return nil
	}
	for _, organization := range *configuration.Organizations {
		if organization.ManagedPocketID == nil {
			continue
		}
		if _, err := fmt.Fprintf(command.OutOrStdout(), "Pocket ID %s: %s\n", organization.Name, organization.ManagedPocketID.IssuerURL); err != nil {
			return err
		}
	}
	if err := printSavedManagementOidc(ctx, command, client, configuration); err != nil {
		if _, err := fmt.Fprintln(command.ErrOrStderr(), "Warning: management configuration applied, but some saved OIDC callback URLs could not be read. Use borq org oidc get NAME to retrieve them."); err != nil {
			return err
		}
	}
	for _, organization := range *configuration.Organizations {
		if organization.ManagedPocketID == nil {
			continue
		}
		if err := printInitialAdministratorEnrollment(command, client, organization.Name, organization.ManagedPocketID.IssuerURL); err != nil {
			return fmt.Errorf("management configuration applied, but initial administrator enrollment for %s is unavailable: %w", organization.Name, err)
		}
	}
	return nil
}

func printSavedManagementOidc(ctx context.Context, command *cobra.Command, client *appapi.ClientWithResponses, configuration appapi.ManagementConfiguration) error {
	declarations := []appapi.ManagementOrganization{}
	for _, organization := range *configuration.Organizations {
		if organization.Oidc != nil || organization.ManagedPocketID != nil {
			declarations = append(declarations, organization)
		}
	}
	if len(declarations) == 0 {
		return nil
	}
	response, err := client.ListOrganizations(ctx)
	organizations, err := readManagedResponse[[]appapi.Organization](response, err)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(command.OutOrStdout(), "\nRegister every callback and logout URL below with its identity provider."); err != nil {
		return err
	}
	for _, declared := range declarations {
		issuer, email, publicOrigin, privateOrigin := "", "", "", ""
		if configuration.BorqPublicURL != nil {
			publicOrigin = *configuration.BorqPublicURL
		}
		if declared.Oidc != nil {
			if configuration.Network != nil && configuration.Network.ManagementURL != nil {
				privateOrigin = *configuration.Network.ManagementURL
			}
			email = declared.Oidc.AdminEmail
			if declared.Oidc.IssuerURL != nil {
				issuer = *declared.Oidc.IssuerURL
			}
			if declared.Oidc.BaseURL != nil && *declared.Oidc.BaseURL != "" {
				publicOrigin = *declared.Oidc.BaseURL
			}
		} else {
			issuer, email = declared.ManagedPocketID.IssuerURL, declared.ManagedPocketID.AdminEmail
			if declared.ManagedPocketID.OrganizationAdminEmail != nil {
				email = *declared.ManagedPocketID.OrganizationAdminEmail
			}
			if declared.ManagedPocketID.BaseURL != nil && *declared.ManagedPocketID.BaseURL != "" {
				publicOrigin = *declared.ManagedPocketID.BaseURL
			}
		}
		if _, err := fmt.Fprintf(command.OutOrStdout(), "\nOrganization %s\n  OIDC issuer: %s\n  Administrator: %s\n", declared.Name, issuer, email); err != nil {
			return err
		}
		var identity appapi.Organization
		for _, organization := range organizations {
			if organization.Name == declared.Name {
				identity = organization
				break
			}
		}
		for _, address := range []struct{ label, origin string }{{"Public", publicOrigin}, {"Private", privateOrigin}} {
			if address.origin == "" {
				continue
			}
			callback, err := previewOrganizationOidcCallback(identity, issuer, address.origin)
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintf(command.OutOrStdout(), "  %s sign-in: %s?organization=%s\n  %s callback: %s\n  %s logout: %s\n", address.label, callback.LogoutCallbackURL, url.QueryEscape(declared.Name), address.label, callback.CallbackURL, address.label, callback.LogoutCallbackURL); err != nil {
				return err
			}
		}
	}
	return nil
}

func printInitialAdministratorEnrollment(command *cobra.Command, client *appapi.ClientWithResponses, organization, issuer string) error {
	response, err := client.CreateOrganizationPocketIDAdminEnrollmentWithResponse(command.Context(), organization)
	if err != nil {
		return err
	}
	if response.JSON200 == nil {
		return applicationFailure(response.StatusCode(), response.Body)
	}
	if response.JSON200.Enrolled {
		_, err := fmt.Fprintf(command.OutOrStdout(), "Pocket ID %s: administrator passkey is already registered.\n", organization)
		return err
	}
	if response.JSON200.URL == nil {
		return errors.New("server returned no initial administrator enrollment link")
	}
	location, err := url.Parse(*response.JSON200.URL)
	provider, providerErr := url.Parse(issuer)
	if err != nil || providerErr != nil || location.Scheme != "https" || location.Host == "" || location.Host != provider.Host || location.User != nil || location.Fragment != "" {
		return errors.New("server returned an invalid Pocket ID enrollment URL")
	}
	_, err = fmt.Fprintf(command.OutOrStdout(), "Register the initial administrator's passkey for %s by opening this private, temporary, single-use link in a browser:\n%s\nAfter registering, return to Borq to sign in. No email setup is required.\n", organization, location.String())
	return err
}
