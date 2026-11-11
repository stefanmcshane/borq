package cli

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/stefanmcshane/borq/api/appapi"
	"github.com/stefanmcshane/borq/foundations/oidc"
)

type organizationOidcCallback struct {
	// Organization identifies the organization whose provider will receive this URL.
	Organization string `json:"organization"`
	// CallbackURL is the redirect URI to register with the selected issuer.
	CallbackURL string `json:"callbackUrl"`
	// LogoutCallbackURL returns the browser to Borq after provider logout.
	LogoutCallbackURL string `json:"logoutCallbackUrl"`
}

func newOrganizationOidcCommand() *cobra.Command {
	group := &cobra.Command{Use: "oidc", Short: "Read organization sign-in settings and provider callback URLs."}
	readScope := applicationScope{}
	read := &cobra.Command{Use: "get NAME", Short: "Read saved OIDC settings without client secrets.", Args: cobra.ExactArgs(1), RunE: func(command *cobra.Command, arguments []string) error {
		client, err := applicationClient(readScope.server)
		if err != nil {
			return err
		}
		response, err := client.GetOrganizationOidc(command.Context(), arguments[0])
		settings, err := readManagedResponse[appapi.OrganizationOidc](response, err)
		if err != nil {
			return err
		}
		return printApplicationValue(command.OutOrStdout(), settings)
	}}
	addApplicationServerFlag(read, &readScope)
	callbackScope := applicationScope{}
	issuer, base := "", ""
	callback := &cobra.Command{Use: "callback NAME", Short: "Preview callback URLs before saving OIDC credentials or contacting the provider.", Args: cobra.ExactArgs(1), RunE: func(command *cobra.Command, arguments []string) error {
		client, err := applicationClient(callbackScope.server)
		if err != nil {
			return err
		}
		response, err := client.ListOrganizations(command.Context())
		organizations, err := readManagedResponse[[]appapi.Organization](response, err)
		if err != nil {
			return err
		}
		for _, organization := range organizations {
			if organization.Name != arguments[0] {
				continue
			}
			value, err := previewOrganizationOidcCallback(organization, issuer, base)
			if err != nil {
				return err
			}
			return printApplicationValue(command.OutOrStdout(), value)
		}
		return errors.New("organization not found; create it with borq org create NAME before previewing its callback")
	}}
	addApplicationServerFlag(callback, &callbackScope)
	callback.Flags().StringVar(&issuer, "issuer-url", "", "Exact OIDC issuer URL, including any tenant path.")
	callback.Flags().StringVar(&base, "borq-public-url", "", "Browser-facing Borq origin without a path.")
	_ = callback.MarkFlagRequired("issuer-url")
	_ = callback.MarkFlagRequired("borq-public-url")
	group.AddCommand(read, callback, newOrganizationOidcEnrollAdminCommand())
	return group
}

func previewOrganizationOidcCallback(organization appapi.Organization, issuer, base string) (organizationOidcCallback, error) {
	issuer, base = strings.TrimSpace(issuer), strings.TrimSpace(base)
	if len(organization.ID) != 32 || strings.Trim(organization.ID, "0123456789abcdef") != "" {
		return organizationOidcCallback{}, errors.New("server returned no valid immutable organization identity")
	}
	for _, address := range []string{issuer, base} {
		if err := oidc.ValidateURL(address); err != nil {
			return organizationOidcCallback{}, err
		}
	}
	address, _ := url.Parse(base)
	if address.Path != "" && address.Path != "/" || address.RawPath != "" {
		return organizationOidcCallback{}, errors.New("Borq public URL must be an origin without a path")
	}
	base = strings.TrimRight(base, "/")
	// Match the server and browser preview: the immutable owner and exact issuer
	// bind the redirect URI without discovery, credentials, or a successful apply.
	digest := sha256.Sum256([]byte(organization.ID + "/" + issuer))
	return organizationOidcCallback{Organization: organization.Name,
		CallbackURL:       fmt.Sprintf("%s/api/auth/oidc/callback/%x", base, digest[:16]),
		LogoutCallbackURL: base + "/login"}, nil
}

func printManagementOidcCallbacks(ctx context.Context, command *cobra.Command, client *appapi.ClientWithResponses, configuration appapi.ManagementConfiguration) error {
	if configuration.Organizations == nil {
		return nil
	}
	declarations := []appapi.ManagementOrganization{}
	for _, organization := range *configuration.Organizations {
		if organization.Oidc != nil {
			declarations = append(declarations, organization)
		}
	}
	if len(declarations) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	response, err := client.ListOrganizations(ctx)
	organizations, err := readManagedResponse[[]appapi.Organization](response, err)
	if err != nil {
		return err
	}
	base := ""
	if configuration.BorqPublicURL != nil {
		base = strings.TrimSpace(*configuration.BorqPublicURL)
	}
	for _, declared := range declarations {
		if base != "" || declared.Oidc.BaseURL != nil && strings.TrimSpace(*declared.Oidc.BaseURL) != "" {
			continue
		}
		response, err := client.GetManagementConfiguration(ctx)
		saved, err := readManagedResponse[appapi.ManagementConfiguration](response, err)
		if err != nil {
			return err
		}
		if saved.BorqPublicURL != nil {
			base = *saved.BorqPublicURL
		}
		break
	}
	for _, declared := range declarations {
		found := false
		issuer, origin := "", base
		if declared.Oidc.IssuerURL != nil {
			issuer = *declared.Oidc.IssuerURL
		}
		if declared.Oidc.BaseURL != nil && strings.TrimSpace(*declared.Oidc.BaseURL) != "" {
			origin = *declared.Oidc.BaseURL
		}
		for _, organization := range organizations {
			if organization.Name != declared.Name {
				continue
			}
			value, err := previewOrganizationOidcCallback(organization, issuer, origin)
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintf(command.ErrOrStderr(), "OIDC %s callback: %s\nOIDC %s logout callback: %s\n", value.Organization, value.CallbackURL, value.Organization, value.LogoutCallbackURL); err != nil {
				return err
			}
			found = true
			break
		}
		if !found {
			return errors.New("accepted organization's identity is unavailable")
		}
	}
	return nil
}

func newOrganizationOidcEnrollAdminCommand() *cobra.Command {
	scope := applicationScope{}
	subject := ""
	command := &cobra.Command{Use: "enroll-admin NAME", Short: "Enroll the first administrator by account ID using the installation admin token.", Args: cobra.ExactArgs(1), RunE: func(command *cobra.Command, arguments []string) error {
		client, err := applicationClient(scope.server)
		if err != nil {
			return err
		}
		response, err := client.GetOrganizationOidc(command.Context(), arguments[0])
		settings, err := readManagedResponse[appapi.OrganizationOidc](response, err)
		if err != nil {
			return err
		}
		result, err := client.EnrollOrganizationOidcAdministrator(command.Context(), arguments[0], appapi.OrganizationOidcAdministratorInput{Subject: subject, Revision: settings.Revision})
		permissions, err := readManagedResponse[appapi.OrganizationPermissions](result, err)
		if err != nil {
			return err
		}
		return printApplicationValue(command.OutOrStdout(), permissions)
	}}
	addApplicationServerFlag(command, &scope)
	command.Flags().StringVar(&subject, "subject", "", "Exact Account ID from Borq's successful Test sign-in result.")
	_ = command.MarkFlagRequired("subject")
	return command
}
