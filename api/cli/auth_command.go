package cli

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/stefanmcshane/borq/business/cli/localconfig"
	"github.com/stefanmcshane/borq/business/cli/management"
	"golang.org/x/term"
)

func newLoginCommand() *cobra.Command {
	server, tokenFile := "", ""
	command := &cobra.Command{Use: "login", Short: "Authenticate the CLI with a server API token.", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error { return executeLogin(command, server, tokenFile) },
	}
	command.Flags().StringVar(&server, "api-url", "", "Borq API URL; defaults to the selected profile.")
	command.Flags().StringVar(&tokenFile, "token-file", "", "Private API token or management recovery file; reads only the Borq token. Omit to enter a token privately.")
	return command
}

func newLogoutCommand() *cobra.Command {
	server := ""
	command := &cobra.Command{Use: "logout", Short: "Remove the saved CLI credential for the selected server.", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			endpoint, err := authenticationEndpoint(command, server)
			if err != nil {
				return err
			}
			path, err := credentialPath(endpoint)
			if err != nil {
				return err
			}
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("remove CLI credential: %w", err)
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "Logged out of %s\n", endpoint)
			return err
		},
	}
	command.Flags().StringVar(&server, "api-url", "", "Borq API URL; defaults to the selected profile.")
	return command
}

func authenticationEndpoint(command *cobra.Command, server string) (string, error) {
	if !command.Flags().Changed("api-url") {
		settings, err := selectedClientSettings(command)
		if err != nil {
			return "", err
		}
		server = settings.Server
	}
	return canonicalServerOrigin(server)
}

func executeLogin(command *cobra.Command, server, tokenFile string) error {
	endpoint, err := authenticationEndpoint(command, server)
	if err != nil {
		return err
	}
	if err := validateCredentialTransport(endpoint); err != nil {
		return err
	}
	var token string
	if command.Flags().Changed("token-file") && tokenFile == "" {
		return errors.New("--token-file must name a private token or management recovery file")
	}
	if tokenFile != "" {
		token, err = readLoginToken(tokenFile)
	} else {
		token, err = promptLoginToken(command)
	}
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(command.Context(), http.MethodGet, endpoint+"/api/auth/session", nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	client := http.Client{Timeout: 30 * time.Second, CheckRedirect: refuseAuthenticationRedirect}
	response, err := client.Do(request)
	if err != nil {
		return errors.New("cannot contact Borq to validate the API token")
	}
	defer response.Body.Close()
	var session struct {
		Authenticated bool `json:"authenticated"`
	}
	if response.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&session) != nil || !session.Authenticated {
		return errors.New("API token was rejected; check the selected server and token file")
	}
	path, err := credentialPath(endpoint)
	if err != nil {
		return err
	}
	if err := savePrivateCredential(path, token); err != nil {
		return err
	}
	if command.Flags().Changed("api-url") {
		if err := selectLoginServer(command, endpoint); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(command.OutOrStdout(), "Logged in to %s\n", endpoint)
	if err != nil {
		return err
	}
	return printLoginOrganizationGuidance(command, endpoint, token)
}

func printLoginOrganizationGuidance(command *cobra.Command, endpoint, token string) error {
	client, err := applicationClientWithToken(endpoint, token)
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(command.Context(), 5*time.Second)
	defer cancel()
	organizations, err := client.ListOrganizationsWithResponse(ctx)
	// Login remains successful when this account cannot list organizations.
	if err != nil || organizations.JSON200 == nil || len(*organizations.JSON200) != 0 {
		return nil
	}
	_, err = fmt.Fprint(command.OutOrStdout(), `
No organizations yet. Create your first organization:
  borq org create NAME

Use your administrator token until you configure organization sign-in.
To use your own OIDC provider:
  borq org create --template=oidc > organization.yaml
  # Edit organization.yaml with your administrator email, issuer, client credentials and public Borq URL.
  borq org create NAME --file organization.yaml
`)
	return err
}

func validateCredentialTransport(origin string) error {
	endpoint, err := url.Parse(origin)
	if err != nil {
		return err
	}
	host := endpoint.Hostname()
	if endpoint.Scheme != "https" && !(endpoint.Scheme == "http" && (host == "localhost" || net.ParseIP(host).IsLoopback())) {
		return errors.New("API tokens require HTTPS; HTTP is allowed only for localhost or a loopback address")
	}
	return nil
}

func selectLoginServer(command *cobra.Command, endpoint string) error {
	settings, path, err := loadClientSettings()
	if err != nil {
		return err
	}
	name, err := requestedClientProfile(command, settings, path)
	if err != nil {
		return err
	}
	if name == "" {
		settings.Server = endpoint
		return saveClientSettings(path, settings, false)
	}
	profile, err := readClientProfile(path, name)
	if err != nil {
		return err
	}
	profile.Server = &endpoint
	return saveClientProfile(path, name, profile, settings)
}

func canonicalServerOrigin(server string) (string, error) {
	endpoint, err := url.Parse(server)
	if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || (endpoint.Path != "" && endpoint.Path != "/") {
		return "", errors.New("API URL must be an http or https server origin without credentials, paths, query parameters or fragments")
	}
	hostname, port := strings.ToLower(endpoint.Hostname()), endpoint.Port()
	if (endpoint.Scheme == "http" && port == "80") || (endpoint.Scheme == "https" && port == "443") {
		port = ""
	}
	host := hostname
	if port != "" {
		host = net.JoinHostPort(hostname, port)
	} else if strings.Contains(hostname, ":") {
		host = "[" + hostname + "]"
	}
	return endpoint.Scheme + "://" + host, nil
}

func credentialPath(origin string) (string, error) {
	directory, err := localconfig.Directory()
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(origin))
	return filepath.Join(directory, "credentials", hex.EncodeToString(digest[:])+".token"), nil
}

func readPrivateTokenData(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read API token file: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("API token file must be a regular file accessible only by its owner (chmod 600)")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("cannot read API token file or file exceeds size limit")
	}
	return data, nil
}

func validateLoginToken(token string) (string, error) {
	token = strings.TrimSpace(token)
	if len(token) < 32 || len(token) > 4096 || strings.ContainsAny(token, " \t\r\n") {
		return "", errors.New("API token must contain 32 to 4096 characters without whitespace")
	}
	return token, nil
}

func readPrivateToken(path string) (string, error) {
	data, err := readPrivateTokenData(path, 4096)
	if err != nil {
		return "", err
	}
	return validateLoginToken(string(data))
}

func readLoginToken(path string) (string, error) {
	data, err := readPrivateTokenData(path, 2<<20)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(strings.TrimSpace(string(data)), "{") {
		return management.ReadAdminToken(path)
	}
	return validateLoginToken(string(data))
}

func promptLoginToken(command *cobra.Command) (string, error) {
	_, _ = fmt.Fprint(command.ErrOrStderr(), "Borq API token (input hidden): ")
	input := command.InOrStdin()
	if file, ok := input.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		data, err := term.ReadPassword(int(file.Fd()))
		_, _ = fmt.Fprintln(command.ErrOrStderr())
		if err != nil {
			return "", errors.New("cannot read API token; use --token-file with a private token or management recovery file")
		}
		return validateLoginToken(string(data))
	}
	data, err := bufio.NewReader(io.LimitReader(input, 4097)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", errors.New("cannot read API token; use --token-file with a private token or management recovery file")
	}
	return validateLoginToken(data)
}

func savePrivateCredential(path, token string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create CLI credentials directory: %w", err)
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".credential-*")
	if err != nil {
		return fmt.Errorf("create private CLI credential: %w", err)
	}
	defer os.Remove(file.Name())
	if _, err := file.WriteString(token + "\n"); err != nil {
		_ = file.Close()
		return fmt.Errorf("write CLI credential: %w", err)
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return fmt.Errorf("save CLI credential: %w", err)
	}
	return nil
}

// Both login and subsequent API calls stop before forwarding administrator credentials.
func refuseAuthenticationRedirect(_ *http.Request, _ []*http.Request) error {
	return http.ErrUseLastResponse
}
