//go:build integration

package appserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// TestReleaseOidc exercises the candidate CLI and server together: saved callback
// guidance must survive unrelated applies, and both configured hosts must sign in.
func TestReleaseOidc(t *testing.T) {
	binary := os.Getenv("BORQ_RELEASE_BINARY")
	if binary == "" {
		t.Skip("BORQ_RELEASE_BINARY selects the explicit release artifact")
	}
	assert := is.New(t)
	binary, err := filepath.Abs(binary)
	assert.NoErr(err)
	provider, kubeconfig, exchanges := oidcProviderFixture(t, true)
	directory := t.TempDir()
	environment := []string{}
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "BORQ_") && !strings.HasPrefix(entry, "OTEL_") && !strings.HasPrefix(entry, "XDG_CONFIG_HOME=") {
			environment = append(environment, entry)
		}
	}
	environment = append(environment, "XDG_CONFIG_HOME="+filepath.Join(directory, "config"))
	run := func(t *testing.T, arguments ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, binary, arguments...)
		command.Env, command.Dir, command.WaitDelay = environment, directory, time.Second
		var output, diagnostic oidcReleaseOutput
		command.Stdout, command.Stderr = &output, &diagnostic
		if err := command.Run(); err != nil {
			t.Fatalf("release command %s failed: %v; %s", arguments[0], err, strings.ReplaceAll(diagnostic.String(), "private-client-secret", "[redacted]"))
		}
		return output.String()
	}
	t.Logf("Candidate: %s", strings.TrimSpace(run(t, "--version")))
	public, private := "https://borq.example.test", "https://borq-default.pheasant-rainbow.ts.net"
	// Seed only trusted routing observations. The binary owns organization setup,
	// secret storage, durable operations, and all browser authentication behavior.
	database, err := sqlite.New(t.Context(), filepath.Join(directory, "borq.sqlite"))
	assert.NoErr(err)
	writes := []sqlite.Write{}
	for collection, value := range map[string]any{
		"management-network-settings": map[string]any{"borqPublicUrl": public, "network": platform.NetworkSettings{Tailscale: &platform.TailscaleSettings{}}},
		"management-network-state":    platform.NetworkState{ManagementURL: private},
	} {
		encoded, err := json.Marshal(value)
		assert.NoErr(err)
		writes = append(writes, sqlite.Write{Collection: collection, Key: "shared", Value: encoded})
	}
	assert.NoErr(database.Commit(t.Context(), writes))
	assert.NoErr(database.Close())
	base := startReleaseOidcServer(t, binary, directory, kubeconfig, environment)
	tokenFile := filepath.Join(directory, "admin-token")
	run(t, "login", "--api-url", base, "--token-file", tokenFile)
	configuration := filepath.Join(directory, "management.yaml")
	assert.NoErr(os.WriteFile(configuration, []byte(fmt.Sprintf("organizations:\n  - name: team\n    managedAddons: false\n    oidc:\n      issuerUrl: %s\n      clientId: borq-client\n      clientSecret: private-client-secret\n      borqPublicUrl: %s\n      adminEmail: owner@example.test\n", provider.URL, public)), 0600))
	first := run(t, "management", "apply", "--api-url", base, "-f", configuration, "--timeout", "15s")
	assert.NoErr(os.WriteFile(configuration, []byte("components: {}\n"), 0600))
	repeated := run(t, "management", "apply", "--api-url", base, "-f", configuration, "--timeout", "15s")
	client := http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	token, err := os.ReadFile(tokenFile)
	assert.NoErr(err)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/api/v1/organizations/team/oidc", nil)
	assert.NoErr(err)
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	response, err := client.Do(request)
	assert.NoErr(err)
	var settings struct {
		CallbackURL string `json:"callbackUrl"`
	}
	assert.Equal(response.StatusCode, http.StatusOK)
	assert.NoErr(json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&settings))
	assert.NoErr(response.Body.Close())
	assert.True(strings.HasPrefix(settings.CallbackURL, public+"/api/auth/oidc/callback/"))
	callbackPath := strings.TrimPrefix(settings.CallbackURL, public)
	t.Run("final_saved_callback_summary", func(t *testing.T) {
		for name, output := range map[string]string{"initial apply": first, "apply without OIDC": repeated} {
			for _, want := range []string{"Management configuration applied.", "Public callback: " + public + callbackPath, "Private callback: " + private + callbackPath, "Public logout: " + public + "/login", "Private logout: " + private + "/login"} {
				if !strings.Contains(output, want) {
					t.Errorf("%s missing final saved guidance %q", name, want)
				}
			}
			for _, unwanted := range []string{"id: \"\"", "status: \"\"", "resources:", "private-client-secret"} {
				if strings.Contains(output, unwanted) {
					t.Errorf("%s contains operation YAML or private credential material", name)
				}
			}
		}
	})
	for _, origin := range []string{public, private} {
		t.Run("normal_login_"+strings.TrimPrefix(origin, "https://"), func(t *testing.T) {
			assert := is.New(t)
			call := func(method, path string, body io.Reader, cookies ...*http.Cookie) *http.Response {
				t.Helper()
				request, err := http.NewRequestWithContext(t.Context(), method, base+path, body)
				assert.NoErr(err)
				request.Host = strings.TrimPrefix(origin, "https://")
				request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				for _, cookie := range cookies {
					request.AddCookie(cookie)
				}
				response, err := client.Do(request)
				assert.NoErr(err)
				assert.NoErr(response.Body.Close())
				return response
			}
			start := call(http.MethodPost, "/api/auth/oidc/login", strings.NewReader("organization=team"))
			if start.StatusCode != http.StatusSeeOther {
				t.Fatalf("configured %s host rejected normal sign-in before provider redirect: got %d, want 303", origin, start.StatusCode)
			}
			cookies := start.Cookies()
			assert.Equal(len(cookies), 1)
			assert.True(cookies[0].Secure && cookies[0].HttpOnly && cookies[0].Domain == "")
			authorize, err := client.Get(start.Header.Get("Location"))
			assert.NoErr(err)
			assert.NoErr(authorize.Body.Close())
			assert.Equal(authorize.StatusCode, http.StatusFound)
			callback, err := url.Parse(authorize.Header.Get("Location"))
			assert.NoErr(err)
			assert.Equal(callback.Scheme+"://"+callback.Host+callback.Path, origin+callbackPath)
			before := exchanges.Load()
			// Host-only secure cookies are replayed manually over the local HTTP
			// connection, matching TLS termination by the configured reverse proxy.
			completed := call(http.MethodGet, callback.RequestURI(), nil, cookies[0])
			assert.Equal(completed.StatusCode, http.StatusSeeOther)
			assert.Equal(exchanges.Load(), before+1)
			var session *http.Cookie // net/http exposes cookie identity through pointers.
			for _, cookie := range completed.Cookies() {
				if cookie.Name == "borq-session" {
					session = cookie
				}
			}
			assert.True(session != nil && session.Secure && session.HttpOnly)
			assert.Equal(call(http.MethodGet, "/account", nil, session).StatusCode, http.StatusOK)
		})
	}
}

// startReleaseOidcServer bounds a separate server process and waits for its real
// HTTP listener. Every dependency belongs to this test's temporary fixture.
func startReleaseOidcServer(t *testing.T, binary, directory, kubeconfig string, environment []string) string {
	t.Helper()
	assert := is.New(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	assert.NoErr(err)
	port := listener.Addr().(*net.TCPAddr).Port
	assert.NoErr(listener.Close())
	ctx, cancel := context.WithCancel(t.Context())
	command := exec.CommandContext(ctx, binary, "server", "--listen", "127.0.0.1", "--port", strconv.Itoa(port), "--data-dir", directory, "--infrastructure-enabled=false", "--management-enabled=false", "--management-kubeconfig", kubeconfig)
	command.Env, command.Dir, command.WaitDelay = environment, directory, time.Second
	var logs oidcReleaseOutput
	command.Stdout, command.Stderr = &logs, &logs
	assert.NoErr(command.Start())
	finished := make(chan error, 1)
	go func() { finished <- command.Wait() }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(3 * time.Second):
			t.Error("release server did not stop after cancellation")
		}
	})
	base := "http://127.0.0.1:" + strconv.Itoa(port)
	client := http.Client{Timeout: 200 * time.Millisecond}
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for {
		response, err := client.Get(base + "/api/livez")
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return base
			}
		}
		select {
		case err := <-finished:
			finished <- err
			t.Fatalf("release server exited before listening: %v; %s", err, logs.String())
		case <-deadline.C:
			t.Fatal("release server did not start listening within 10 seconds")
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// Process output is shared with os/exec's copy goroutines and capped at 64 KiB.
type oidcReleaseOutput struct {
	mutex sync.Mutex
	bytes bytes.Buffer
}

func (output *oidcReleaseOutput) Write(value []byte) (int, error) {
	output.mutex.Lock()
	defer output.mutex.Unlock()
	_, _ = output.bytes.Write(value[:min(len(value), (64<<10)-output.bytes.Len())])
	return len(value), nil
}

func (output *oidcReleaseOutput) String() string {
	output.mutex.Lock()
	defer output.mutex.Unlock()
	return output.bytes.String()
}
