package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/api/appapi"
	"github.com/stefanmcshane/borq/foundations/logger"
)

// TestApplyReplansAcceptedSidecars covers older sidecars as well as new ones:
// completed applies never block another apply, and unchanged active retries reuse their operation.
func TestApplyReplansAcceptedSidecars(t *testing.T) {
	for _, status := range []appapi.OperationStatus{appapi.Pending, appapi.Queued, appapi.Applying, appapi.Waiting, appapi.Canceling, appapi.Succeeded, appapi.Failed, appapi.Canceled, appapi.Superseded} {
		for _, changed := range []bool{false, true} {
			name := string(status) + "/unchanged"
			if changed {
				name = string(status) + "/changed"
			}
			t.Run(name, func(t *testing.T) {
				assert := is.New(t)
				var paths []string
				var diagnostics strings.Builder
				fresh := changed || status == appapi.Succeeded || status == appapi.Failed || status == appapi.Canceled || status == appapi.Superseded
				server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					paths = append(paths, request.URL.Path)
					writer.Header().Set("Content-Type", "application/json")
					switch request.URL.Path {
					case "/api/v1/operations/old":
						_ = json.NewEncoder(writer).Encode(appapi.Operation{ID: "old", PlanID: "old", Status: status})
					case "/api/v1/organizations/team/spaces/dev/plans":
						writer.WriteHeader(http.StatusCreated)
						_ = json.NewEncoder(writer).Encode(appapi.Plan{ID: "new", ConfigurationDiff: "+ replicas: 2\n"})
					case "/api/v1/plans/new/apply", "/api/v1/plans/old/apply":
						if fresh {
							assert.True(strings.Contains(diagnostics.String(), "+ replicas: 2"))
						}
						writer.WriteHeader(http.StatusAccepted)
						_ = json.NewEncoder(writer).Encode(appapi.Operation{ID: "accepted", Status: appapi.Pending})
					default:
						t.Errorf("unexpected request: %s", request.URL.Path)
						writer.WriteHeader(http.StatusNotFound)
					}
				}))
				defer server.Close()
				file := filepath.Join(t.TempDir(), "apps.yaml")
				source := []byte("apps:\n- name: api\n  replicas: 1\n")
				assert.NoErr(os.WriteFile(file, source, 0600))
				options := deploymentOptions{file: file, scope: applicationScope{server: server.URL, organization: "team", space: "dev"}}
				assert.NoErr(saveApplicationPlan(applicationPlanPath(options), applicationPlanMetadata(options, source, "old")))
				if changed {
					assert.NoErr(os.WriteFile(file, []byte("apps:\n- name: api\n  replicas: 2\n"), 0600))
				}
				t.Setenv("XDG_CONFIG_HOME", t.TempDir())
				command := NewRootCommand(context.Background(), logger.Logger{}, Config{Port: 7833, DataDir: t.TempDir()})
				var output strings.Builder
				command.SetOut(&output)
				command.SetErr(&diagnostics)
				command.SetArgs([]string{"apply", "--api-url", server.URL, "--organization", "team", "--space", "dev", "-f", file})
				assert.NoErr(command.Execute())
				if fresh {
					expected := []string{"/api/v1/organizations/team/spaces/dev/plans", "/api/v1/plans/new/apply"}
					if !changed {
						expected = append([]string{"/api/v1/operations/old"}, expected...)
					}
					assert.Equal(paths, expected)
				} else {
					assert.Equal(paths, []string{"/api/v1/operations/old", "/api/v1/plans/old/apply"})
				}
			})
		}
	}
}

// TestApplyDoesNotReplanOnLookupFailure prevents API errors from being mistaken
// for a completed rollout and silently replacing a reviewed plan.
func TestApplyDoesNotReplanOnLookupFailure(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			assert := is.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				assert.Equal(request.Method, http.MethodGet)
				assert.Equal(request.URL.Path, "/api/v1/operations/old")
				writer.WriteHeader(status)
			}))
			defer server.Close()
			file := filepath.Join(t.TempDir(), "apps.yaml")
			source := []byte("apps: []\n")
			assert.NoErr(os.WriteFile(file, source, 0600))
			options := deploymentOptions{file: file, scope: applicationScope{server: server.URL, organization: "team", space: "dev"}}
			assert.NoErr(saveApplicationPlan(applicationPlanPath(options), applicationPlanMetadata(options, source, "old")))
			_, _, err := runApplicationCommand(t, "apply", "--api-url", server.URL, "--organization", "team", "--space", "dev", "-f", file)
			assert.True(err != nil)
		})
	}
}

// TestUnsubmittedMismatchedPlansDoNotBlockApply verifies stale sidecars from older
// clients do not block an ordinary apply after changing input or deployment targets.
func TestUnsubmittedMismatchedPlansDoNotBlockApply(t *testing.T) {
	for _, change := range []string{"file", "organization", "space", "server"} {
		t.Run(change, func(t *testing.T) {
			assert := is.New(t)
			var paths []string
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				paths = append(paths, request.URL.Path)
				writer.Header().Set("Content-Type", "application/json")
				switch request.URL.Path {
				case "/api/v1/operations/old":
					writer.WriteHeader(http.StatusNotFound)
					_ = json.NewEncoder(writer).Encode(appapi.Problem{Message: "operation not found"})
				case "/api/v1/organizations/team/spaces/dev/plans":
					writer.WriteHeader(http.StatusCreated)
					_ = json.NewEncoder(writer).Encode(appapi.Plan{ID: "new"})
				case "/api/v1/plans/new/apply":
					writer.WriteHeader(http.StatusAccepted)
					_ = json.NewEncoder(writer).Encode(appapi.Operation{ID: "new", PlanID: "new", Status: appapi.Pending})
				default:
					t.Errorf("unexpected request: %s", request.URL.Path)
					writer.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			file := filepath.Join(t.TempDir(), "apps.yaml")
			source := []byte("apps: []\n")
			assert.NoErr(os.WriteFile(file, source, 0600))
			options := deploymentOptions{file: file, scope: applicationScope{server: server.URL, organization: "team", space: "dev"}}
			metadata := applicationPlanMetadata(options, source, "old")
			switch change {
			case "file":
				assert.NoErr(os.WriteFile(file, []byte("# changed input\napps: []\n"), 0600))
			case "organization":
				metadata.Organization = "previous"
			case "space":
				metadata.Space = "previous"
			case "server":
				metadata.Server = "http://127.0.0.1:1"
			}
			assert.NoErr(saveApplicationPlan(applicationPlanPath(options), metadata))
			_, diagnostics, err := runApplicationCommand(t, "apply", "--api-url", server.URL, "--organization", "team", "--space", "dev", "-f", file)
			assert.NoErr(err)
			assert.True(strings.Contains(diagnostics, "Plan new"))
			expected := []string{"/api/v1/organizations/team/spaces/dev/plans", "/api/v1/plans/new/apply"}

			assert.Equal(paths, expected)
		})
	}
}
