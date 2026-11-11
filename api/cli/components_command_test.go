package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/api/appapi"
)

// TestClusterLinkPreviewsThenQueues keeps registration, read-only comparison,
// and asynchronous setup separate without polling installation completion.
func TestClusterLinkPreviewsThenQueues(t *testing.T) {
	for _, input := range []struct {
		name            string
		flags           []string
		diff, reconcile bool
	}{
		{name: "registration-only", flags: []string{"--reconcile=false", "--diff=false"}},
		{name: "preview-only", flags: []string{"--reconcile=false"}, diff: true},
		{name: "default", diff: true, reconcile: true},
		{name: "preview-bypass", flags: []string{"--diff=false"}, reconcile: true},
	} {
		t.Run(input.name, func(t *testing.T) {
			assert := is.New(t)
			requests := []string{}
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				requests = append(requests, request.Method+" "+request.URL.Path)
				writer.Header().Set("Content-Type", "application/json")
				switch {
				case strings.HasSuffix(request.URL.Path, "/clusters"):
					var link appapi.LinkClusterInput
					assert.NoErr(json.NewDecoder(request.Body).Decode(&link))
					assert.Equal(link.Name, "target")
					assert.Equal(request.Method, http.MethodPost)
					body := link.Connection
					assert.True(body.Reconcile != nil)
					assert.Equal(*body.Reconcile, false)
					_ = json.NewEncoder(writer).Encode(appapi.Cluster{Organization: "team", Name: "target-abc12"})
				case request.Method == http.MethodGet:
					assert.True(strings.HasSuffix(request.URL.Path, "/components/diff"))
					_ = json.NewEncoder(writer).Encode(clusterTestDiff())
				case request.Method == http.MethodPost:
					assert.True(strings.HasSuffix(request.URL.Path, "/components/reconcile"))
					_ = json.NewEncoder(writer).Encode(appapi.ComponentInstallation{ID: "run-1", Status: "queued"})
				}
			}))
			defer server.Close()
			flags := []string{"cluster", "link", "target", "--organization", "team", "--server", server.URL, "--kubeconfig", clusterLinkTestKubeconfig(t)}
			output, diagnostics, err := runApplicationCommand(t, append(flags, input.flags...)...)
			assert.NoErr(err)
			expected := []string{"POST /api/v1/organizations/team/clusters"}
			if input.diff {
				expected = append(expected, "GET /api/v1/organizations/team/clusters/target-abc12/components/diff")
				assert.True(strings.Contains(diagnostics, "Missing target component:"))
				assert.True(!strings.Contains(diagnostics, "valuesDiff:"))
			}
			if input.reconcile {
				expected = append(expected, "POST /api/v1/organizations/team/clusters/target-abc12/components/reconcile")
				assert.True(strings.Contains(output, "target components queued"))
			} else {
				assert.True(strings.Contains(output, "Cluster target-abc12 linked"))
			}
			assert.Equal(requests, expected)
		})
	}
}

// TestClusterLinkDoesNotEnqueueAfterPreviewFailure leaves a successful link
// intact while surfacing conflicts and comparison failures without another registration.
func TestClusterLinkDoesNotEnqueueAfterPreviewFailure(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		assert := is.New(t)
		var registrations, posts atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writer.Header().Set("Content-Type", "application/json")
			if strings.HasSuffix(request.URL.Path, "/clusters") {
				registrations.Add(1)
				_ = json.NewEncoder(writer).Encode(appapi.Cluster{Organization: "team", Name: "target-abc12"})
				return
			}
			if request.Method == http.MethodPost {
				posts.Add(1)
			}
			if conflict && strings.HasSuffix(request.URL.Path, "/diff") {
				_ = json.NewEncoder(writer).Encode(appapi.ComponentDiff{Components: []appapi.ComponentChange{{Name: "traefik", Action: "conflict", Reason: "installed in kube-system"}}})
				return
			}
			writer.WriteHeader(http.StatusInternalServerError)
			_, _ = writer.Write([]byte(`{"message":"cluster comparison failed"}`))
		}))
		flags := []string{"cluster", "link", "target", "--organization", "team", "--server", server.URL, "--kubeconfig", clusterLinkTestKubeconfig(t), "--output", "yaml"}
		output, _, err := runApplicationCommand(t, flags...)
		assert.True(err != nil)
		assert.True(strings.Contains(err.Error(), "cluster target-abc12 is linked"))
		assert.True(strings.Contains(output, "name: target-abc12"))
		assert.Equal(registrations.Load(), int32(1))
		assert.Equal(posts.Load(), int32(0))
		server.Close()
	}
}

// TestClusterLinkOnlySkipsPreviewForDisabledInfrastructure prevents an ordinary
// registration-only status from hiding network or ownership comparison failures.
func TestClusterLinkOnlySkipsPreviewForDisabledInfrastructure(t *testing.T) {
	for _, input := range []struct {
		status  int
		message string
		skip    bool
	}{
		{http.StatusBadRequest, "infrastructure management is disabled", true},
		{http.StatusBadRequest, "cluster registered without scheduling component reconciliation", false},
		{http.StatusInternalServerError, "infrastructure management is disabled", false},
	} {
		assert := is.New(t)
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writer.Header().Set("Content-Type", "application/json")
			switch {
			case strings.HasSuffix(request.URL.Path, "/clusters"):
				_ = json.NewEncoder(writer).Encode(appapi.Cluster{Organization: "team", Name: "target-abc12"})
			case strings.HasSuffix(request.URL.Path, "/diff"):
				writer.WriteHeader(input.status)
				_, _ = writer.Write([]byte(`{"message":"comparison unavailable"}`))
			case request.Method == http.MethodGet:
				_ = json.NewEncoder(writer).Encode(appapi.ComponentInstallation{Status: "disabled", Message: input.message})
			default:
				t.Errorf("unexpected setup request %s %s", request.Method, request.URL.Path)
			}
		}))
		_, _, err := runApplicationCommand(t, "cluster", "link", "target", "--organization", "team", "--server", server.URL, "--kubeconfig", clusterLinkTestKubeconfig(t))
		assert.Equal(err == nil, input.skip)
		server.Close()
	}
}

// TestClusterComponentsWaitsWithoutRequeue prints the saved diff once and follows
// one queued reconciliation or upgrade through readiness without another POST.
func TestClusterComponentsWaitsWithoutRequeue(t *testing.T) {
	for _, action := range []string{"reconcile", "upgrade"} {
		t.Run(action, func(t *testing.T) {
			assert := is.New(t)
			var posts, polls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				if request.Method == http.MethodPost {
					assert.True(strings.HasSuffix(request.URL.Path, "/"+action))
					posts.Add(1)
					_ = json.NewEncoder(writer).Encode(appapi.ComponentInstallation{ID: "run-1", Status: "queued"})
					return
				}
				status := "installing"
				if polls.Add(1) == 2 {
					status = "ready"
				}
				_ = json.NewEncoder(writer).Encode(appapi.ComponentInstallation{ID: "run-1", Status: status, Diff: clusterTestDiff()})
			}))
			defer server.Close()
			output, diagnostics, err := runApplicationCommand(t, "cluster", action, "target", "--organization", "team", "--server", server.URL)
			assert.NoErr(err)
			assert.Equal(posts.Load(), int32(1))
			assert.Equal(polls.Load(), int32(2))
			assert.True(strings.Contains(output, "status: ready"))
			assert.True(!strings.Contains(output, "Components queued:"))
			assert.Equal(strings.Count(diagnostics, "Missing target component:"), 1)
		})
	}
}

// TestClusterWaitTimeoutDoesNotResubmit keeps local wait deadlines independent
// from server execution and retains the last observed durable run in stdout.
func TestClusterWaitTimeoutDoesNotResubmit(t *testing.T) {
	assert := is.New(t)
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.Method == http.MethodPost {
			posts.Add(1)
		}
		_ = json.NewEncoder(writer).Encode(appapi.ComponentInstallation{ID: "continuing-run", Status: "installing"})
	}))
	defer server.Close()
	output, _, err := runApplicationCommand(t, "cluster", "reconcile", "target", "--organization", "team", "--server", server.URL, "--timeout", "10ms")
	assert.True(err != nil)
	assert.True(strings.Contains(err.Error(), "context deadline exceeded"))
	assert.True(strings.Contains(output, "id: continuing-run"))
	assert.Equal(posts.Load(), int32(1))
}

// TestClusterDiffIsReadOnly checks both normal and explicit-upgrade comparisons
// use the read-only generated endpoint rather than scheduling component work.
func TestClusterDiffIsReadOnly(t *testing.T) {
	for _, upgrade := range []bool{false, true} {
		assert := is.New(t)
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			assert.Equal(request.Method, http.MethodGet)
			assert.Equal(request.URL.Path, "/api/v1/organizations/team/clusters/target/components/diff")
			assert.Equal(request.URL.Query().Get("upgrade") == "true", upgrade)
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(clusterTestDiff())
		}))
		flags := []string{"cluster", "diff", "target", "--organization", "team", "--server", server.URL}
		if upgrade {
			flags = append(flags, "--upgrade")
		}
		output, _, err := runApplicationCommand(t, flags...)
		assert.NoErr(err)
		assert.True(strings.Contains(output, "desiredVersion:"))
		server.Close()
	}
}

func clusterTestDiff() appapi.ComponentDiff {
	return appapi.ComponentDiff{Components: []appapi.ComponentChange{{Name: "traefik", Chart: "traefik", DesiredVersion: "42.0.0", Action: "install", ValuesDiff: "+ replicas: 1"}}}
}
