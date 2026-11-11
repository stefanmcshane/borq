//go:build integration

package pocketid

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/matryer/is"
)

// TestManagedPocketIdProtocol proves the provisioning and retry contracts against Pocket ID v2.19.1 itself.
// The Kubernetes lifecycle belongs to platform tests; this test owns upstream API compatibility and durable provider state.
func TestManagedPocketIdProtocol(t *testing.T) {
	is := is.New(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	dataPath := t.TempDir()
	is.NoErr(os.Chmod(dataPath, 0777))
	// Pre-create the only upload directory so the nonroot container can write and the test user can clean it up.
	is.NoErr(os.Mkdir(filepath.Join(dataPath, "uploads"), 0777))
	is.NoErr(os.Chmod(filepath.Join(dataPath, "uploads"), 0777))
	is.NoErr(os.Mkdir(filepath.Join(dataPath, "uploads", "application-images"), 0777))
	is.NoErr(os.Chmod(filepath.Join(dataPath, "uploads", "application-images"), 0777))
	name := "borq-pocketid-protocol-" + uuid.NewString()
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cleanupCancel()
		if err := exec.CommandContext(cleanupCtx, "docker", "rm", "-f", name).Run(); err != nil {
			t.Errorf("remove disposable Pocket ID container: %v", err)
		}
	})
	apiKey := "protocol-test-static-key-" + uuid.NewString()
	docker(ctx, t, "run", "--detach", "--name", name, "--publish", "127.0.0.1::1411", "--mount", "type=bind,source="+dataPath+",target=/app/data",
		"--env", "APP_URL=http://localhost:1411", "--env", "ENCRYPTION_KEY=protocol-test-encryption-"+uuid.NewString(), "--env", "STATIC_API_KEY="+apiKey,
		"--env", "UI_CONFIG_DISABLED=true", "--env", "AUTO_CREATE_OIDC_CLIENT_SECRET=false", "--env", "ANALYTICS_DISABLED=true", "--env", "VERSION_CHECK_DISABLED=true",
		"ghcr.io/pocket-id/pocket-id:v2.19.1-distroless@sha256:d035c9c229304cfd25f97a79b2da00bde9cf942abbd70430d452a95420e2836b")
	baseURL := "http://" + strings.TrimSpace(docker(ctx, t, "port", name, "1411/tcp"))
	waitForPocketId(ctx, t, baseURL)
	client, err := New(Config{BaseURL: baseURL, ApiKey: apiKey})
	is.NoErr(err)
	admin := Admin{ID: uuid.NewString(), Username: "borq-admin", Email: "owner@example.com"}
	configuration := OidcClient{ID: strings.ReplaceAll(uuid.NewString(), "-", ""), Name: "Borq", CallbackURL: "https://borq.example.com/api/auth/oidc/callback/protocol-test", LogoutCallbackURL: "https://borq.example.com/login", LaunchURL: "https://borq.example.com/", Secret: uuid.NewString() + uuid.NewString()}

	// Even an authenticated static API user does not make it safe to publish the first-admin endpoint.
	status, _ := protocolRequest(ctx, t, baseURL, apiKey, http.MethodGet, "/api/users/me", nil)
	is.Equal(status, http.StatusOK)
	status, _ = protocolRequest(ctx, t, baseURL, "", http.MethodGet, "/api/signup/setup", nil)
	is.Equal(status, http.StatusNoContent)
	is.NoErr(client.EnsureAdmin(ctx, admin))
	status, _ = protocolRequest(ctx, t, baseURL, "", http.MethodPost, "/api/signup/setup", map[string]any{"username": "intruder", "email": "intruder@example.com", "firstName": "Intruder"})
	is.Equal(status, http.StatusConflict)
	is.NoErr(client.EnsureClient(ctx, configuration))
	is.NoErr(client.VerifyClient(ctx, configuration))
	status, body := protocolRequest(ctx, t, baseURL, apiKey, http.MethodGet, "/api/oidc/users/me/clients?filters%5BhasLaunchURL%5D=true", nil)
	is.Equal(status, http.StatusOK)
	var launcher struct {
		Data []clientResponse `json:"data"`
	}
	is.NoErr(json.Unmarshal(body, &launcher))
	is.Equal(len(launcher.Data), 1)
	is.Equal(launcher.Data[0].ID, configuration.ID)
	is.Equal(launcher.Data[0].LaunchURL, configuration.LaunchURL)
	wrongSecret := configuration
	wrongSecret.Secret = "different-secret-" + uuid.NewString()
	usable, err := client.secretUsable(ctx, wrongSecret)
	is.NoErr(err)
	is.True(!usable)
	is.True(client.VerifyClient(ctx, wrongSecret) != nil)
	assertProvisionedOnce(ctx, t, baseURL, apiKey, admin, configuration)

	// Retrying after credentials were already committed must not create users, clients, or duplicate secrets.
	is.NoErr(client.EnsureAdmin(ctx, admin))
	is.NoErr(client.EnsureClient(ctx, configuration))
	assertProvisionedOnce(ctx, t, baseURL, apiKey, admin, configuration)

	docker(ctx, t, "restart", "--time", "5", name)
	baseURL = "http://" + strings.TrimSpace(docker(ctx, t, "port", name, "1411/tcp"))
	waitForPocketId(ctx, t, baseURL)
	client, err = New(Config{BaseURL: baseURL, ApiKey: apiKey})
	is.NoErr(err)
	is.NoErr(client.EnsureAdmin(ctx, admin))
	is.NoErr(client.EnsureClient(ctx, configuration))
	assertProvisionedOnce(ctx, t, baseURL, apiKey, admin, configuration)

	t.Run("backfill preserves provider settings", func(t *testing.T) {
		assert := is.New(t)
		endpoint := "/api/oidc/clients/" + configuration.ID
		status, body := protocolRequest(ctx, t, baseURL, apiKey, http.MethodGet, endpoint, nil)
		assert.Equal(status, http.StatusOK)
		var settings map[string]any
		assert.NoErr(json.Unmarshal(body, &settings))
		settings["name"] = "Administrator display name"
		settings["description"] = "Administrator description"
		settings["launchURL"] = nil
		settings["logoutCallbackURLs"] = []string{"https://other.example.com/logout"}
		settings["backchannelLogoutURL"] = "https://other.example.com/backchannel"
		settings["requiresReauthentication"] = true
		settings["requiresPushedAuthorizationRequests"] = true
		settings["skipConsent"] = true
		settings["isGroupRestricted"] = true
		settings["accessTokenDurationMinutes"] = 15
		settings["refreshTokenDurationMinutes"] = 90
		settings["credentials"] = map[string]any{"federatedIdentities": []any{map[string]any{"issuer": "https://federation.example.com", "subject": "allowed-workload", "audience": "borq", "jwks": "https://federation.example.com/keys", "replayProtection": true}}}
		status, _ = protocolRequest(ctx, t, baseURL, apiKey, http.MethodPut, endpoint, settings)
		assert.Equal(status, http.StatusOK)
		status, body = protocolRequest(ctx, t, baseURL, apiKey, http.MethodPost, "/api/user-groups", map[string]string{"name": "allowed-borq-users", "friendlyName": "Allowed Borq users"})
		assert.Equal(status, http.StatusCreated)
		var group struct {
			ID string `json:"id"`
		}
		assert.NoErr(json.Unmarshal(body, &group))
		assert.True(group.ID != "")
		status, _ = protocolRequest(ctx, t, baseURL, apiKey, http.MethodPut, endpoint+"/allowed-user-groups", map[string]any{"userGroupIds": []string{group.ID}})
		assert.Equal(status, http.StatusOK)
		input := configuration
		input.Name, input.Secret = "", ""
		assert.NoErr(client.EnsureClientLinks(ctx, input))
		assert.NoErr(client.EnsureClientLinks(ctx, input))
		status, body = protocolRequest(ctx, t, baseURL, apiKey, http.MethodGet, endpoint, nil)
		assert.Equal(status, http.StatusOK)
		var saved map[string]any
		assert.NoErr(json.Unmarshal(body, &saved))
		assert.Equal(saved["launchURL"], configuration.LaunchURL)
		assert.Equal(saved["logoutCallbackURLs"], []any{"https://other.example.com/logout", configuration.LogoutCallbackURL})
		for _, field := range []string{"name", "description", "backchannelLogoutURL", "requiresReauthentication", "requiresPushedAuthorizationRequests", "skipConsent", "isGroupRestricted", "accessTokenDurationMinutes", "refreshTokenDurationMinutes"} {
			original, err := json.Marshal(settings[field])
			assert.NoErr(err)
			actual, err := json.Marshal(saved[field])
			assert.NoErr(err)
			assert.Equal(string(actual), string(original))
		}
		credentials := saved["credentials"].(map[string]any)
		assert.Equal(credentials["federatedIdentities"].([]any)[0].(map[string]any)["subject"], "allowed-workload")
		assert.Equal(saved["allowedUserGroups"].([]any)[0].(map[string]any)["id"], group.ID)
		usable, err := client.secretUsable(ctx, configuration)
		assert.NoErr(err)
		assert.True(usable)
		assert.NoErr(client.VerifyClient(ctx, configuration))
		assertProvisionedOnce(ctx, t, baseURL, apiKey, admin, configuration)
	})

	hasPasskey, err := client.HasPasskey(ctx, admin.ID)
	is.NoErr(err)
	is.True(!hasPasskey)
	token, err := client.NewOneTimeAccess(ctx, admin.ID)
	is.NoErr(err)
	status, body = protocolRequest(ctx, t, baseURL, "", http.MethodPost, "/api/one-time-access-token/"+token, nil)
	is.Equal(status, http.StatusOK)
	var signedIn userResponse
	is.NoErr(json.Unmarshal(body, &signedIn))
	is.Equal(signedIn.ID, admin.ID)
	is.True(signedIn.IsAdmin)
	status, _ = protocolRequest(ctx, t, baseURL, "", http.MethodPost, "/api/one-time-access-token/"+token, nil)
	is.Equal(status, http.StatusUnauthorized)

	// Organization membership provisions an ordinary user, even after bootstrap.
	member := User{ID: uuid.NewString(), Username: "borq-member", Email: "member@example.com"}
	is.NoErr(client.EnsureUser(ctx, member))
	is.NoErr(client.EnsureUser(ctx, member))
	is.True(client.EnsureAdmin(ctx, admin) != nil)
	status, body = protocolRequest(ctx, t, baseURL, apiKey, http.MethodGet, "/api/users/"+member.ID, nil)
	is.Equal(status, http.StatusOK)
	var normal userResponse
	is.NoErr(json.Unmarshal(body, &normal))
	is.Equal(normal.ID, member.ID)
	is.True(!normal.IsAdmin)
	enrollment, err := client.NewOneTimeAccess(ctx, member.ID)
	is.NoErr(err)
	status, body = protocolRequest(ctx, t, baseURL, "", http.MethodPost, "/api/one-time-access-token/"+enrollment, nil)
	is.Equal(status, http.StatusOK)
	is.NoErr(json.Unmarshal(body, &normal))
	is.Equal(normal.ID, member.ID)
	is.True(!normal.IsAdmin)
	status, _ = protocolRequest(ctx, t, baseURL, "", http.MethodPost, "/api/one-time-access-token/"+enrollment, nil)
	is.Equal(status, http.StatusUnauthorized)
}

func assertProvisionedOnce(ctx context.Context, t *testing.T, baseURL, apiKey string, admin Admin, client OidcClient) {
	t.Helper()
	is := is.New(t)
	status, body := protocolRequest(ctx, t, baseURL, apiKey, http.MethodGet, "/api/users", nil)
	is.Equal(status, http.StatusOK)
	var users struct {
		Data []userResponse `json:"data"`
	}
	is.NoErr(json.Unmarshal(body, &users))
	var realUsers []userResponse
	for _, user := range users.Data {
		if user.ID != "00000000-0000-0000-0000-000000000000" {
			realUsers = append(realUsers, user)
		}
	}
	is.Equal(len(realUsers), 1)
	is.Equal(realUsers[0].ID, admin.ID)
	status, body = protocolRequest(ctx, t, baseURL, apiKey, http.MethodGet, "/api/oidc/clients", nil)
	is.Equal(status, http.StatusOK)
	var clients struct {
		Data []clientResponse `json:"data"`
	}
	is.NoErr(json.Unmarshal(body, &clients))
	is.Equal(len(clients.Data), 1)
	is.Equal(clients.Data[0].ID, client.ID)
	is.Equal(clients.Data[0].CallbackURLs, []string{client.CallbackURL})
	is.True(slices.Contains(clients.Data[0].LogoutCallbackURLs, client.LogoutCallbackURL))
	is.True(clients.Data[0].LaunchURL != "")
	is.True(clients.Data[0].PkceEnabled)
	is.True(!clients.Data[0].IsPublic)
	status, body = protocolRequest(ctx, t, baseURL, apiKey, http.MethodGet, "/api/oidc/clients/"+client.ID+"/secrets", nil)
	is.Equal(status, http.StatusOK)
	var secrets []struct {
		Active bool `json:"isActive"`
	}
	is.NoErr(json.Unmarshal(body, &secrets))
	is.Equal(len(secrets), 1)
	is.True(secrets[0].Active)
	is.True(!bytes.Contains(body, []byte(client.Secret)))
}

func protocolRequest(ctx context.Context, t *testing.T, baseURL, apiKey, method, endpoint string, input any) (int, []byte) {
	t.Helper()
	is := is.New(t)
	var body bytes.Buffer
	if input != nil {
		is.NoErr(json.NewEncoder(&body).Encode(input))
	}
	request, err := http.NewRequestWithContext(ctx, method, baseURL+endpoint, &body)
	is.NoErr(err)
	request.Header.Set("X-API-KEY", apiKey)
	request.Header.Set("Content-Type", "application/json")
	client := http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal("Pocket ID protocol request failed")
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	is.NoErr(err)
	return response.StatusCode, payload
}

func docker(ctx context.Context, t *testing.T, args ...string) string {
	t.Helper()
	command := exec.CommandContext(ctx, "docker", args...)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		t.Fatalf("Pocket ID test container operation failed: %v; %s", err, stderr.String())
	}
	return string(output)
}

func waitForPocketId(ctx context.Context, t *testing.T, baseURL string) {
	t.Helper()
	deadline, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	client := http.Client{Timeout: time.Second}
	for {
		request, err := http.NewRequestWithContext(deadline, http.MethodGet, baseURL+"/healthz", nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(request)
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusNoContent {
				return
			}
		}
		select {
		case <-deadline.Done():
			t.Fatal(fmt.Errorf("Pocket ID did not become healthy: %w", deadline.Err()))
		case <-ticker.C:
		}
	}
}
