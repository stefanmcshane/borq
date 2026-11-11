// Package pocketid provisions users and OIDC clients through Pocket ID's administrative API.
package pocketid

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/stefanmcshane/borq/foundations/oidc"
)

// Config connects to the private service endpoint; it is distinct from the public OIDC issuer.
type Config struct {
	// BaseURL may include a Kubernetes service proxy path.
	BaseURL string
	// ApiKey is the instance's management key and must remain private.
	ApiKey string `json:"-"`
	// HttpClient supplies the Kubernetes transport, including its TLS and authentication configuration.
	// The pointer preserves the standard client's transport without copying its internal state.
	HttpClient *http.Client `json:"-"`
}

// Client uses a bounded HTTP transport that never follows endpoint redirects.
type Client struct {
	baseURL string
	apiKey  string
	// HTTP clients contain synchronization state and are shared through a pointer.
	httpClient *http.Client
}

// Admin identifies the real initial administrator, separate from Pocket ID's static API user.
type Admin struct {
	// ID is a UUID persisted before the first create request, allowing safe retries.
	ID string
	// Username is the initial administrator's login name.
	Username string
	// Email is the initial administrator's contact address.
	Email string
}

// OidcClient describes a confidential authorization-code client with PKCE required.
type OidcClient struct {
	// ID is persisted before creation so a retry cannot register another client.
	ID string
	// Name labels this client in Pocket ID.
	Name string
	// CallbackURL is the single exact redirect URI accepted for this client.
	CallbackURL string
	// LogoutCallbackURL is an exact permitted post-logout destination.
	LogoutCallbackURL string
	// LaunchURL is the default destination shown in Pocket ID's application launcher.
	LaunchURL string
	// Secret must be durably stored by the caller before provisioning begins.
	Secret string `json:"-"`
}

type userResponse struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Email    string `json:"email"`
	IsAdmin  bool   `json:"isAdmin"`
	Disabled bool   `json:"disabled"`
}

type clientResponse struct {
	ID                                  string          `json:"id"`
	Name                                string          `json:"name"`
	CallbackURLs                        []string        `json:"callbackURLs"`
	IsPublic                            bool            `json:"isPublic"`
	PkceEnabled                         bool            `json:"pkceEnabled"`
	LogoutCallbackURLs                  []string        `json:"logoutCallbackURLs"`
	LaunchURL                           string          `json:"launchURL"`
	Description                         string          `json:"description"`
	BackchannelLogoutURL                string          `json:"backchannelLogoutURL"`
	RequiresReauthentication            bool            `json:"requiresReauthentication"`
	RequiresPushedAuthorizationRequests bool            `json:"requiresPushedAuthorizationRequests"`
	SkipConsent                         bool            `json:"skipConsent"`
	Credentials                         json.RawMessage `json:"credentials"`
	HasLogo                             bool            `json:"hasLogo"`
	HasDarkLogo                         bool            `json:"hasDarkLogo"`
	IsGroupRestricted                   bool            `json:"isGroupRestricted"`
	AccessTokenDurationMinutes          int64           `json:"accessTokenDurationMinutes"`
	RefreshTokenDurationMinutes         int64           `json:"refreshTokenDurationMinutes"`
}

// Pocket ID's PUT replaces settings, including federated credentials and group
// restrictions. Every writable setting from v2.19.1 must survive a link update;
// client secrets and allowed user groups use separate endpoints and are untouched.
type clientUpdate struct {
	Name                                string          `json:"name"`
	Description                         string          `json:"description"`
	CallbackURLs                        []string        `json:"callbackURLs"`
	LogoutCallbackURLs                  []string        `json:"logoutCallbackURLs"`
	BackchannelLogoutURL                string          `json:"backchannelLogoutURL"`
	IsPublic                            bool            `json:"isPublic"`
	PkceEnabled                         bool            `json:"pkceEnabled"`
	RequiresReauthentication            bool            `json:"requiresReauthentication"`
	RequiresPushedAuthorizationRequests bool            `json:"requiresPushedAuthorizationRequests"`
	SkipConsent                         bool            `json:"skipConsent"`
	Credentials                         json.RawMessage `json:"credentials"`
	LaunchURL                           string          `json:"launchURL"`
	HasLogo                             bool            `json:"hasLogo"`
	HasDarkLogo                         bool            `json:"hasDarkLogo"`
	IsGroupRestricted                   bool            `json:"isGroupRestricted"`
	AccessTokenDurationMinutes          int64           `json:"accessTokenDurationMinutes"`
	RefreshTokenDurationMinutes         int64           `json:"refreshTokenDurationMinutes"`
}

type responseError struct {
	status int
}

func (err responseError) Error() string {
	return "Pocket ID rejected the provisioning request; check the managed instance and its credentials"
}

// New builds the administrative client without forwarding credentials through redirects.
func New(config Config) (Client, error) {
	if err := oidc.ValidateURL(config.BaseURL); err != nil {
		return Client{}, errors.New("the Pocket ID service endpoint must use HTTPS or a local loopback address")
	}
	if len(config.ApiKey) < 16 || strings.ContainsAny(config.ApiKey, "\r\n") {
		return Client{}, errors.New("a valid Pocket ID management key is required")
	}
	var transport http.RoundTripper
	if config.HttpClient != nil {
		transport = config.HttpClient.Transport
	}
	return Client{
		baseURL: strings.TrimRight(config.BaseURL, "/"), apiKey: config.ApiKey,
		httpClient: &http.Client{Transport: transport, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
	}, nil
}

// EnsureAdmin bootstraps the persisted initial administrator without adopting other accounts.
// It must not reconcile established instances: only the expected administrator and static API user may exist.
// The caller must also permanently block public first-user signup, including access from other cluster workloads.
func (client Client) EnsureAdmin(ctx context.Context, admin Admin) error {
	if !validUserID(admin.ID) || strings.TrimSpace(admin.Username) == "" || strings.TrimSpace(admin.Email) == "" {
		return errors.New("the Pocket ID administrator requires a persisted UUID, username, and email")
	}
	var user userResponse
	err := client.request(ctx, http.MethodGet, "/api/users/"+admin.ID, nil, &user)
	if statusIs(err, http.StatusNotFound) {
		if err := client.ensureInitialUsers(ctx, admin.ID); err != nil {
			return err
		}
		input := userResponse{ID: admin.ID, Username: admin.Username, Email: admin.Email, IsAdmin: true}
		err = client.request(ctx, http.MethodPost, "/api/users", input, &user)
		if err != nil {
			// The create may have committed before the connection failed. Only the persisted ID is recoverable.
			if recoverErr := client.request(ctx, http.MethodGet, "/api/users/"+admin.ID, nil, &user); recoverErr != nil {
				return err
			}
		}
	} else if err != nil {
		return err
	}
	if user.ID != admin.ID || user.Username != admin.Username || user.Email != admin.Email || !user.IsAdmin || user.Disabled {
		return errors.New("the existing Pocket ID administrator does not match this provisioning request")
	}
	if err := client.ensureInitialUsers(ctx, admin.ID); err != nil {
		return err
	}
	// Verify Pocket ID's first-user endpoint is closed before the caller publishes public ingress.
	err = client.request(ctx, http.MethodGet, "/api/signup/setup", nil, nil)
	if !statusIs(err, http.StatusNotFound) {
		return errors.New("Pocket ID has not closed initial administrator setup")
	}
	return nil
}

func (client Client) ensureInitialUsers(ctx context.Context, adminID string) error {
	var users struct {
		Data       []userResponse `json:"data"`
		Pagination struct {
			TotalItems  int `json:"totalItems"`
			TotalPages  int `json:"totalPages"`
			CurrentPage int `json:"currentPage"`
		} `json:"pagination"`
	}
	// At most the synthetic static-key user and the expected real administrator may exist during bootstrap.
	// Checking the total as well as the rows fails closed if additional users are hidden on another page.
	if err := client.request(ctx, http.MethodGet, "/api/users?pagination%5Blimit%5D=3", nil, &users); err != nil {
		return err
	}
	if users.Pagination.TotalItems > 2 || users.Pagination.TotalItems != len(users.Data) || users.Pagination.TotalPages != 1 || users.Pagination.CurrentPage != 1 {
		return errors.New("Pocket ID has unexpected users during initial administrator setup")
	}
	for _, user := range users.Data {
		if user.ID != "00000000-0000-0000-0000-000000000000" && user.ID != adminID {
			return errors.New("Pocket ID has unexpected users during initial administrator setup")
		}
	}
	return nil
}

// EnsureClient registers the exact callback and preserves one durable client secret across retries.
// Configure Pocket ID with UI_CONFIG_DISABLED=true and AUTO_CREATE_OIDC_CLIENT_SECRET=false
// to avoid an unused generated secret; Pocket ID ignores that setting's environment variable otherwise.
func (client Client) EnsureClient(ctx context.Context, input OidcClient) error {
	if !validClientID(input.ID) || strings.TrimSpace(input.Name) == "" || len(input.Name) > 50 || !validSecret(input.Secret) {
		return errors.New("the Pocket ID client requires an ID, name, and a durable secret of at least 16 printable characters")
	}
	if err := validateClientLinks(input); err != nil {
		return err
	}
	var existing clientResponse
	endpoint := "/api/oidc/clients/" + input.ID
	err := client.request(ctx, http.MethodGet, endpoint, nil, &existing)
	if statusIs(err, http.StatusNotFound) {
		payload := clientResponse{ID: input.ID, Name: input.Name, CallbackURLs: []string{input.CallbackURL}, PkceEnabled: true,
			LogoutCallbackURLs: []string{input.LogoutCallbackURL}, LaunchURL: input.LaunchURL}
		err = client.request(ctx, http.MethodPost, "/api/oidc/clients", payload, nil)
		if readErr := client.request(ctx, http.MethodGet, endpoint, nil, &existing); readErr != nil {
			if err != nil {
				return err
			}
			return readErr
		}
	} else if err != nil {
		return err
	}
	if existing.ID != input.ID || existing.Name != input.Name || existing.IsPublic || !existing.PkceEnabled || len(existing.CallbackURLs) != 1 || existing.CallbackURLs[0] != input.CallbackURL {
		return errors.New("the existing Pocket ID client does not match this provisioning request")
	}
	if err := client.reconcileClientLinks(ctx, input, existing); err != nil {
		return err
	}
	usable, err := client.secretUsable(ctx, input)
	if err != nil || usable {
		return err
	}
	payload := struct {
		Secret string `json:"secret"`
	}{Secret: input.Secret}
	createErr := client.request(ctx, http.MethodPost, endpoint+"/secrets", payload, nil)
	usable, err = client.secretUsable(ctx, input)
	if err == nil && usable {
		return nil
	}
	if createErr != nil {
		return createErr
	}
	return errors.New("Pocket ID did not accept the stored OIDC client secret")
}

// VerifyClient checks the persisted client identity and secret without changing
// provider state. Ready-instance recovery must not recreate accounts, register a
// replacement client, rotate secrets, or overwrite administrator settings.
func (client Client) VerifyClient(ctx context.Context, input OidcClient) error {
	if !validClientID(input.ID) || !validSecret(input.Secret) {
		return errors.New("the Pocket ID client requires a persisted ID and a durable secret of at least 16 printable characters")
	}
	if err := validateClientLinks(input); err != nil {
		return err
	}
	var existing clientResponse
	if err := client.request(ctx, http.MethodGet, "/api/oidc/clients/"+input.ID, nil, &existing); err != nil {
		return err
	}
	if !ownedClient(existing, input) {
		return errors.New("the existing Pocket ID client does not match this provisioning request")
	}
	usable, err := client.secretUsable(ctx, input)
	if err != nil {
		return err
	}
	if !usable {
		return errors.New("Pocket ID did not accept the stored OIDC client secret")
	}
	return nil
}

// EnsureClientLinks backfills launcher and logout destinations for an existing
// owned client. It never creates a client or reads, verifies, or replaces secrets.
// Administrators may rename the client or customize its launcher destination.
func (client Client) EnsureClientLinks(ctx context.Context, input OidcClient) error {
	if !validClientID(input.ID) {
		return errors.New("the Pocket ID client requires a persisted ID")
	}
	if err := validateClientLinks(input); err != nil {
		return err
	}
	var existing clientResponse
	if err := client.request(ctx, http.MethodGet, "/api/oidc/clients/"+input.ID, nil, &existing); err != nil {
		return err
	}
	return client.reconcileClientLinks(ctx, input, existing)
}

func validateClientLinks(input OidcClient) error {
	for _, address := range []string{input.CallbackURL, input.LogoutCallbackURL, input.LaunchURL} {
		if err := oidc.ValidateURL(address); err != nil || strings.Contains(address, "*") {
			return errors.New("Pocket ID callback, logout, and launch destinations must be exact HTTPS URLs or local loopback URLs")
		}
	}
	return nil
}

func ownedClient(existing clientResponse, input OidcClient) bool {
	return existing.ID == input.ID && !existing.IsPublic && existing.PkceEnabled && len(existing.CallbackURLs) == 1 && existing.CallbackURLs[0] == input.CallbackURL
}

func (client Client) reconcileClientLinks(ctx context.Context, input OidcClient, existing clientResponse) error {
	if !ownedClient(existing, input) {
		return errors.New("the existing Pocket ID client does not match this provisioning request")
	}
	launchURL := existing.LaunchURL
	if launchURL == "" {
		launchURL = input.LaunchURL
	}
	logoutURLs := slices.Clone(existing.LogoutCallbackURLs)
	if !slices.Contains(logoutURLs, input.LogoutCallbackURL) {
		logoutURLs = append(logoutURLs, input.LogoutCallbackURL)
	}
	if existing.LaunchURL == launchURL && slices.Equal(existing.LogoutCallbackURLs, logoutURLs) {
		return nil
	}
	inputSettings := clientUpdate{
		Name: existing.Name, Description: existing.Description, CallbackURLs: existing.CallbackURLs,
		LogoutCallbackURLs: logoutURLs, BackchannelLogoutURL: existing.BackchannelLogoutURL,
		IsPublic: existing.IsPublic, PkceEnabled: existing.PkceEnabled,
		RequiresReauthentication: existing.RequiresReauthentication, RequiresPushedAuthorizationRequests: existing.RequiresPushedAuthorizationRequests,
		SkipConsent: existing.SkipConsent, Credentials: existing.Credentials, LaunchURL: launchURL,
		HasLogo: existing.HasLogo, HasDarkLogo: existing.HasDarkLogo, IsGroupRestricted: existing.IsGroupRestricted,
		AccessTokenDurationMinutes: existing.AccessTokenDurationMinutes, RefreshTokenDurationMinutes: existing.RefreshTokenDurationMinutes,
	}
	endpoint := "/api/oidc/clients/" + input.ID
	updateErr := client.request(ctx, http.MethodPut, endpoint, inputSettings, nil)
	// Read after a lost response too: an acknowledged provider write is not the
	// persistence contract, and a retry must never overwrite a stale snapshot.
	var saved clientResponse
	if err := client.request(ctx, http.MethodGet, endpoint, nil, &saved); err != nil {
		return err
	}
	if ownedClient(saved, input) && saved.LaunchURL == launchURL && slices.Contains(saved.LogoutCallbackURLs, input.LogoutCallbackURL) {
		return nil
	}
	if updateErr != nil {
		return updateErr
	}
	return errors.New("Pocket ID did not persist the client launcher and logout destinations")
}

// VerifyAdmin checks retained identity without creating users or resetting their settings.
func (client Client) VerifyAdmin(ctx context.Context, adminID string) error {
	if !validUserID(adminID) {
		return errors.New("a valid Pocket ID administrator ID is required")
	}
	var user userResponse
	if err := client.request(ctx, http.MethodGet, "/api/users/"+adminID, nil, &user); err != nil {
		return err
	}
	if user.ID != adminID || !user.IsAdmin || user.Disabled {
		return errors.New("the retained Pocket ID administrator is missing or disabled")
	}
	return nil
}

// HasPasskey reports whether this exact user has completed passkey enrollment.
func (client Client) HasPasskey(ctx context.Context, adminID string) (bool, error) {
	if !validUserID(adminID) {
		return false, errors.New("a valid Pocket ID administrator ID is required")
	}
	var credentials []struct {
		ID string `json:"id"`
	}
	if err := client.request(ctx, http.MethodGet, "/api/users/"+adminID+"/webauthn-credentials", nil, &credentials); err != nil {
		return false, err
	}
	return len(credentials) > 0, nil
}

// ErrAlreadyEnrolled distinguishes an existing passkey from a provider failure.
var ErrAlreadyEnrolled = errors.New("Pocket ID passkey enrollment is already complete")

// NewOneTimeAccess creates a 30-minute enrollment token only while the user has no passkey.
// The caller must deliver it through a no-store POST response, never persist it or include it in a GET response.
func (client Client) NewOneTimeAccess(ctx context.Context, adminID string) (string, error) {
	hasPasskey, err := client.HasPasskey(ctx, adminID)
	if err != nil {
		return "", err
	}
	if hasPasskey {
		return "", ErrAlreadyEnrolled
	}
	var result struct {
		Token string `json:"token"`
	}
	// Pocket ID uses six-character codes for lifetimes up to 15 minutes; 30 minutes selects its stronger 12-character code.
	payload := struct {
		TTL string `json:"ttl"`
	}{TTL: "30m"}
	if err := client.request(ctx, http.MethodPost, "/api/users/"+adminID+"/one-time-access-token", payload, &result); err != nil {
		return "", err
	}
	if len(result.Token) != 12 || !validPathPart(result.Token) {
		return "", errors.New("Pocket ID returned an invalid enrollment token")
	}
	return result.Token, nil
}

func (client Client) secretUsable(ctx context.Context, input OidcClient) (bool, error) {
	// The token endpoint authenticates the client before rejecting this unusable grant.
	// This proves the complete stored secret, rather than trusting its public prefix, after an interrupted create.
	// client_secret_post also survives the Kubernetes API server's own Authorization header.
	values := url.Values{
		"grant_type": {"authorization_code"}, "client_id": {input.ID}, "client_secret": {input.Secret},
		"code": {"borq-provisioning-credential-check"}, "redirect_uri": {input.CallbackURL},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.baseURL+"/api/oidc/token", strings.NewReader(values.Encode()))
	if err != nil {
		return false, errors.New("cannot build the Pocket ID credential check")
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := client.httpClient.Do(request)
	if err != nil {
		return false, errors.New("cannot reach Pocket ID to verify the client credential")
	}
	defer response.Body.Close()
	var result struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&result); err != nil {
		return false, errors.New("Pocket ID returned an invalid credential-check response")
	}
	if response.StatusCode == http.StatusBadRequest && result.Error == "invalid_grant" {
		return true, nil
	}
	if response.StatusCode == http.StatusUnauthorized && result.Error == "invalid_client" {
		return false, nil
	}
	return false, errors.New("Pocket ID could not verify the stored client credential")
}

func (client Client) request(ctx context.Context, method, endpoint string, input, output any) error {
	var body bytes.Buffer
	if input != nil {
		if err := json.NewEncoder(&body).Encode(input); err != nil {
			return errors.New("cannot encode the Pocket ID provisioning request")
		}
	}
	request, err := http.NewRequestWithContext(ctx, method, client.baseURL+endpoint, &body)
	if err != nil {
		return errors.New("cannot build the Pocket ID provisioning request")
	}
	request.Header.Set("X-API-KEY", client.apiKey)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.httpClient.Do(request)
	if err != nil {
		return errors.New("cannot reach Pocket ID; check the managed instance and service connection")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return responseError{status: response.StatusCode}
	}
	if output == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(output); err != nil {
		return errors.New("Pocket ID returned an invalid provisioning response")
	}
	return nil
}

func statusIs(err error, status int) bool {
	var response responseError
	return errors.As(err, &response) && response.status == status
}

func validUserID(identifier string) bool {
	parsed, err := uuid.Parse(identifier)
	return err == nil && parsed != uuid.Nil && parsed.String() == identifier
}

func validClientID(identifier string) bool {
	return len(identifier) >= 2 && len(identifier) <= 128 && validPathPart(identifier)
}

func validPathPart(value string) bool {
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') && (character < '0' || character > '9') && character != '-' && character != '_' {
			return false
		}
	}
	return value != ""
}

func validSecret(secret string) bool {
	if len(secret) < 16 || len(secret) > 8192 {
		return false
	}
	for _, character := range secret {
		if character < 32 || character > 126 {
			return false
		}
	}
	return true
}
