package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/logger"
)

// TestCliLoginTokenPrompt validates a pasted token and saves the login without
// writing the entered token to command output or diagnostics.
func TestCliLoginTokenPrompt(t *testing.T) {
	assert := is.New(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	token := strings.Repeat("p", 64)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.True(request.Header.Get("Authorization") == "Bearer "+token)
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/api/auth/session" {
			_, _ = writer.Write([]byte(`{"authenticated":true}`))
			return
		}
		_, _ = writer.Write([]byte(`[]`))
	}))
	defer server.Close()
	command := NewRootCommand(context.Background(), logger.Logger{}, Config{})
	var output, diagnostics bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&diagnostics)
	command.SetIn(strings.NewReader(token + "\n"))
	command.SetArgs([]string{"login", "--api-url", server.URL})
	assert.NoErr(command.Execute())
	assert.True(strings.Contains(diagnostics.String(), "Borq API token (input hidden):"))
	assert.True(!strings.Contains(output.String()+diagnostics.String(), token))
	path, err := credentialPath(server.URL)
	assert.NoErr(err)
	saved, err := os.ReadFile(path)
	assert.NoErr(err)
	assert.True(strings.TrimSpace(string(saved)) == token)
}

// TestCliLoginRecoveryFileRequiresBorqToken prevents OpenBao credentials or
// malformed JSON from being treated as an API token.
func TestCliLoginRecoveryFileRequiresBorqToken(t *testing.T) {
	for _, document := range []string{`{"openbao":{"originalToken":"private-vault-token"}}`, `{"borq":{"adminToken":"too-short"}}`, `{"borq":`} {
		assert := is.New(t)
		path := filepath.Join(t.TempDir(), "recovery.json")
		assert.NoErr(os.WriteFile(path, []byte(document), 0600))
		_, err := readLoginToken(path)
		assert.True(err != nil)
		assert.True(!strings.Contains(err.Error(), "private-vault-token"))
	}
}

func TestCliTokenTransportRequiresTls(t *testing.T) {
	assert := is.New(t)
	for _, origin := range []string{"https://borq.example.test", "http://localhost:7833", "http://127.0.0.1:7833", "http://[::1]:7833"} {
		assert.NoErr(validateCredentialTransport(origin))
	}
	assert.True(validateCredentialTransport("http://borq.example.test") != nil)
	_, err := applicationClientWithToken("http://borq.example.test", strings.Repeat("x", 64))
	assert.True(err != nil)
	_, err = applicationClientWithToken("http://borq.example.test", "")
	assert.NoErr(err)
}

// TestCliAdministratorLoginLifecycle owns login verification, durable private
// credentials, automatic request authentication, origin isolation and logout.
func TestCliAdministratorLoginLifecycle(t *testing.T) {
	assert := is.New(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	token := strings.Repeat("a", 64)
	tokenFile := filepath.Join(t.TempDir(), "admin-token")
	assert.NoErr(os.WriteFile(tokenFile, []byte(token+"\n"), 0600))
	var authenticated atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		authenticated.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"authenticated":true,"admin":false}`))
	}))
	defer server.Close()
	output, _, err := runConfiguredApplicationCommand("login", "--api-url", server.URL, "--token-file", tokenFile)
	assert.NoErr(err)
	assert.True(!strings.Contains(output, token))
	settings, _, err := loadClientSettings()
	assert.NoErr(err)
	assert.Equal(settings.Server, server.URL)
	credential, err := credentialPath(server.URL)
	assert.NoErr(err)
	info, err := os.Stat(credential)
	assert.NoErr(err)
	assert.Equal(info.Mode().Perm(), os.FileMode(0600))
	data, err := os.ReadFile(credential)
	assert.NoErr(err)
	assert.Equal(strings.TrimSpace(string(data)), token)
	client, err := applicationClient(server.URL + "/")
	assert.NoErr(err)
	response, err := client.GetManagementConfigurationWithResponse(t.Context())
	assert.NoErr(err)
	assert.Equal(response.StatusCode(), http.StatusOK)
	assert.Equal(authenticated.Load(), int64(3))
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(r.Header.Get("Authorization"), "")
	}))
	defer other.Close()
	otherClient, err := applicationClient(other.URL)
	assert.NoErr(err)
	_, err = otherClient.GetManagementConfigurationWithResponse(t.Context())
	assert.NoErr(err)
	_, _, err = runConfiguredApplicationCommand("logout", "--api-url", server.URL)
	assert.NoErr(err)
	_, err = os.Stat(credential)
	assert.True(os.IsNotExist(err))
	client, err = applicationClient(server.URL)
	assert.NoErr(err)
	_, err = client.GetManagementConfigurationWithResponse(t.Context())
	assert.True(err != nil && strings.Contains(err.Error(), "borq login"))
}

// TestCliAdministratorLoginRejectsUnsafeCredentials owns rejected sessions,
// insecure local token files and refusing credential forwarding on redirects.
func TestCliAdministratorLoginRejectsUnsafeCredentials(t *testing.T) {
	assert := is.New(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	token := strings.Repeat("b", 64)
	file := filepath.Join(t.TempDir(), "token")
	assert.NoErr(os.WriteFile(file, []byte(token), 0600))
	var otherContacts atomic.Int64
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { otherContacts.Add(1) }))
	defer other.Close()
	for _, response := range []string{"rejected", "unauthenticated", "redirect"} {
		t.Run(response, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if response == "redirect" {
					http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
					return
				}
				if response == "rejected" {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				_, _ = w.Write([]byte(`{"authenticated":false,"admin":false}`))
			}))
			defer server.Close()
			_, _, err := runConfiguredApplicationCommand("login", "--api-url", server.URL, "--token-file", file)
			assert.True(err != nil)
			path, err := credentialPath(server.URL)
			assert.NoErr(err)
			_, err = os.Stat(path)
			assert.True(os.IsNotExist(err))
		})
	}
	assert.Equal(otherContacts.Load(), int64(0))
	assert.NoErr(os.Chmod(file, 0644))
	_, err := readPrivateToken(file)
	assert.True(err != nil && strings.Contains(err.Error(), "chmod 600"))
}

// TestApplicationClientStopsCredentialRedirects proves a credential remains
// bound to the selected origin even when the server redirects an API request.
func TestApplicationClientStopsCredentialRedirects(t *testing.T) {
	assert := is.New(t)
	var destinationContacts atomic.Int64
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { destinationContacts.Add(1) }))
	defer destination.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(r.Header.Get("Authorization"), "Bearer "+strings.Repeat("c", 64))
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	client, err := applicationClientWithToken(server.URL, strings.Repeat("c", 64))
	assert.NoErr(err)
	response, err := client.GetManagementConfigurationWithResponse(t.Context())
	assert.NoErr(err)
	assert.Equal(response.StatusCode(), http.StatusTemporaryRedirect)
	assert.Equal(destinationContacts.Load(), int64(0))
}

// TestCliLoginSelectsProfileServer preserves organization, space and cluster
// selections while making an explicit login target the selected profile API.
func TestCliLoginSelectsProfileServer(t *testing.T) {
	assert := is.New(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, arguments := range [][]string{
		{"config", "set", "--api-url", "https://base.example", "--organization", "base", "--space", "base-space", "--cluster", "base-cluster"},
		{"config", "set", "--profile", "testing", "--api-url", "https://old.example", "--organization", "test-org", "--space", "test-space", "--cluster", "test-cluster"},
		{"config", "use", "testing"},
	} {
		_, _, err := runConfiguredApplicationCommand(arguments...)
		assert.NoErr(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"authenticated":true,"admin":false}`))
	}))
	defer server.Close()
	tokenFile := filepath.Join(t.TempDir(), "token")
	assert.NoErr(os.WriteFile(tokenFile, []byte(strings.Repeat("e", 64)), 0600))
	_, _, err := runConfiguredApplicationCommand("login", "--api-url", server.URL, "--token-file", tokenFile)
	assert.NoErr(err)
	base, path, err := loadClientSettings()
	assert.NoErr(err)
	assert.Equal(base.Server, "https://base.example")
	assert.Equal(base.CurrentProfile, "testing")
	profile, err := readClientProfile(path, "testing")
	assert.NoErr(err)
	assert.Equal(*profile.Server, server.URL)
	assert.Equal(*profile.Organization, "test-org")
	assert.Equal(*profile.Space, "test-space")
	assert.Equal(*profile.Cluster, "test-cluster")
}

// TestCliLoginOrganizationGuidance suggests creation and an OIDC override only after a successful empty organization listing.
func TestCliLoginOrganizationGuidance(t *testing.T) {
	for _, test := range []struct {
		name, body string
		status     int
		hint       bool
	}{
		{"empty", "[]", http.StatusOK, true},
		{"existing", `[{"id":"team-id","name":"team","managedAddons":true}]`, http.StatusOK, false},
		{"forbidden", `{"message":"forbidden"}`, http.StatusForbidden, false},
		{"unavailable", `unavailable`, http.StatusServiceUnavailable, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			token := strings.Repeat("x", 64)
			file := filepath.Join(t.TempDir(), "admin.token")
			assert.NoErr(os.WriteFile(file, []byte(token), 0600))
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				assert.Equal(request.Header.Get("Authorization"), "Bearer "+token)
				writer.Header().Set("Content-Type", "application/json")
				if request.URL.Path == "/api/auth/session" {
					_, _ = writer.Write([]byte(`{"authenticated":true}`))
					return
				}
				assert.Equal(request.URL.Path, "/api/v1/organizations")
				writer.WriteHeader(test.status)
				_, _ = writer.Write([]byte(test.body))
			}))
			defer server.Close()
			output, _, err := runConfiguredApplicationCommand("login", "--api-url", server.URL, "--token-file", file)
			assert.NoErr(err)
			assert.True(strings.Contains(output, "Logged in"))
			assert.Equal(strings.Contains(output, "No organizations yet"), test.hint)
			assert.Equal(strings.Contains(output, "borq org create --template=oidc"), test.hint)
			assert.True(!strings.Contains(output, token))
		})
	}
}
