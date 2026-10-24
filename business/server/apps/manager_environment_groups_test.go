package apps

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/matryer/is"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestEnvironmentGroupPublishingReplacesCompleteValues proves publishing is distinct
// from app apply, unchanged maps reuse a version, and omitted keys are deleted.
func TestEnvironmentGroupPublishingReplacesCompleteValues(t *testing.T) {
	assert := is.New(t)
	manager, _, vault, _ := newEnvironmentGroupManager(t)
	target := environmentGroupTestTarget()
	first, err := manager.PublishEnvironmentGroup(t.Context(), PublishEnvironmentGroupInput{Target: target, Name: "backend", Values: []Variable{{Name: "ZZ", Value: "remove"}, {Name: "XX", Value: "first"}}})
	assert.NoErr(err)
	assert.Equal(first, EnvironmentGroup{Name: "backend", Version: 1, Values: []Variable{{Name: "XX", Value: "first"}, {Name: "ZZ", Value: "remove"}}})
	assert.Equal(vault.writes(), 1)
	same, err := manager.PublishEnvironmentGroup(t.Context(), PublishEnvironmentGroupInput{Target: target, Name: "backend", Values: []Variable{{Name: "XX", Value: "first"}, {Name: "ZZ", Value: "remove"}}})
	assert.NoErr(err)
	assert.Equal(same, first)
	assert.Equal(vault.writes(), 1)
	second, err := manager.PublishEnvironmentGroup(t.Context(), PublishEnvironmentGroupInput{Target: target, Name: "backend", Values: []Variable{{Name: "XX", Value: "next"}}})
	assert.NoErr(err)
	assert.Equal(second, EnvironmentGroup{Name: "backend", Version: 2, Values: []Variable{{Name: "XX", Value: "next"}}})
	latest, err := manager.GetEnvironmentGroup(t.Context(), target, "backend", "latest")
	assert.NoErr(err)
	assert.Equal(latest, second)
	pinned, err := manager.GetEnvironmentGroup(t.Context(), target, "backend", "1")
	assert.NoErr(err)
	assert.Equal(pinned, first)
	empty, err := manager.PublishEnvironmentGroup(t.Context(), PublishEnvironmentGroupInput{Target: target, Name: "backend", Values: []Variable{}})
	assert.NoErr(err)
	assert.Equal(empty, EnvironmentGroup{Name: "backend", Version: 3, Values: []Variable{}})
	assert.Equal(vault.writes(), 3)
	assert.Equal(vault.retention(), 11)
	listed, err := manager.ListEnvironmentGroups(t.Context(), target)
	assert.NoErr(err)
	assert.Equal(listed, []EnvironmentGroup{empty})
	plan, err := manager.Plan(t.Context(), managerPlanInput(t, "production", `{"apps":[{"name":"backend","image":"backend:v1","env":{"name":"backend","version":"3"}}]}`))
	assert.NoErr(err)
	assert.Equal(plan.Configuration.Apps[0].Env, EnvReference{Name: "backend", Version: "3"})
	references, err := EnvReferences(plan.Manifests)
	assert.NoErr(err)
	assert.Equal(len(references), 1)
	for _, manifest := range plan.Manifests {
		if manifest["kind"] == "ExternalSecret" {
			keys, _, err := unstructured.NestedString(manifest, "spec", "target", "template", "metadata", "annotations", "borq.run/env-keys")
			assert.NoErr(err)
			assert.Equal(keys, "[]")
		}
	}
	assert.Equal(vault.writes(), 3)
}

// TestEnvironmentGroupCloneCopiesPublishedVersion starts a new independent family and
// never overwrites an existing destination even when its values match.
func TestEnvironmentGroupCloneCopiesPublishedVersion(t *testing.T) {
	assert := is.New(t)
	manager, _, vault, _ := newEnvironmentGroupManager(t)
	target := environmentGroupTestTarget()
	first, err := manager.PublishEnvironmentGroup(t.Context(), PublishEnvironmentGroupInput{Target: target, Name: "backend", Values: []Variable{{Name: "XX", Value: "old"}}})
	assert.NoErr(err)
	_, err = manager.PublishEnvironmentGroup(t.Context(), PublishEnvironmentGroupInput{Target: target, Name: "backend", Values: []Variable{{Name: "XX", Value: "new"}}})
	assert.NoErr(err)
	clone, err := manager.CloneEnvironmentGroup(t.Context(), CloneEnvironmentGroupInput{Target: target, Name: "candidate", SourceName: "backend", SourceVersion: "1"})
	assert.NoErr(err)
	assert.Equal(clone, EnvironmentGroup{Name: "candidate", Version: 1, Values: first.Values})
	_, err = manager.CloneEnvironmentGroup(t.Context(), CloneEnvironmentGroupInput{Target: target, Name: "candidate", SourceName: "backend", SourceVersion: "1"})
	assert.True(errors.Is(err, ErrConflict))
	assert.Equal(vault.writes(), 3)
	configs, err := manager.ListEnvironmentGroups(t.Context(), target)
	assert.NoErr(err)
	assert.Equal(len(configs), 2)
	assert.Equal(configs[0].Name, "backend")
	assert.Equal(configs[1], clone)
	_, err = manager.GetEnvironmentGroup(t.Context(), target, "missing", "latest")
	assert.True(errors.Is(err, ErrNotFound))
}

// TestEnvironmentGroupPublicationConflicts rejects stale edits and a publisher racing
// between the metadata read and the atomic OpenBao check-and-set write.
func TestEnvironmentGroupPublicationConflicts(t *testing.T) {
	assert := is.New(t)
	manager, _, vault, _ := newEnvironmentGroupManager(t)
	target := environmentGroupTestTarget()
	_, err := manager.PublishEnvironmentGroup(t.Context(), PublishEnvironmentGroupInput{Target: target, Name: "backend", Values: []Variable{{Name: "XX", Value: "first"}}})
	assert.NoErr(err)
	zero := 0
	_, err = manager.PublishEnvironmentGroup(t.Context(), PublishEnvironmentGroupInput{Target: target, Name: "backend", Values: []Variable{{Name: "XX", Value: "first"}}, BaseVersion: &zero})
	assert.True(errors.Is(err, ErrConflict))
	assert.Equal(vault.writes(), 1)
	vault.injectConcurrentWrite()
	_, err = manager.PublishEnvironmentGroup(t.Context(), PublishEnvironmentGroupInput{Target: target, Name: "backend", Values: []Variable{{Name: "XX", Value: "lost-write"}}})
	assert.True(errors.Is(err, ErrConflict))
	assert.Equal(vault.writes(), 2)
	latest, err := manager.GetEnvironmentGroup(t.Context(), target, "backend", "latest")
	assert.NoErr(err)
	assert.Equal(latest.Values, []Variable{{Name: "CONCURRENT", Value: "preserved"}})
	assert.Equal(latest.Version, 2)
}

// TestEnvironmentGroupRejectsInvalidPublication fails malformed names, duplicate keys,
// and invalid version selectors before creating any vault versions.
func TestEnvironmentGroupRejectsInvalidPublication(t *testing.T) {
	assert := is.New(t)
	manager, _, vault, _ := newEnvironmentGroupManager(t)
	target := environmentGroupTestTarget()
	for _, input := range []PublishEnvironmentGroupInput{
		{Target: target, Name: "invalid/name"},
		{Target: target, Name: "config", Values: []Variable{{Name: "A"}, {Name: "A"}}},
		{Target: target, Name: "config", Values: []Variable{{Name: "invalid=name"}}},
	} {
		_, err := manager.PublishEnvironmentGroup(t.Context(), input)
		assert.True(errors.Is(err, ErrInvalid))
	}
	for _, version := range []string{"0", "01", "-1", "next"} {
		_, err := manager.GetEnvironmentGroup(t.Context(), target, "config", version)
		assert.True(errors.Is(err, ErrInvalid))
	}
	for _, reference := range []string{`{"name":"missing"}`, `{"name":"missing","version":"1"}`} {
		_, err := manager.Plan(t.Context(), managerPlanInput(t, "production", `{"apps":[{"name":"backend","image":"backend:v1","env":`+reference+`}]}`))
		assert.True(errors.Is(err, ErrNotFound))
	}
	assert.Equal(vault.writes(), 0)
}

func environmentGroupTestTarget() EnvironmentGroupTarget {
	return EnvironmentGroupTarget{Organization: "acme", Space: "production"}
}

// Shared HTTP requests mutate vault version history, so pointer ownership keeps
// the mutex and write counters together for the lifetime of the test server.
type environmentGroupVault struct {
	mutex           sync.Mutex
	versions        map[string][]map[string]string
	writeCount      int
	maxVersions     int
	concurrentWrite bool
}

func newEnvironmentGroupManager(t *testing.T) (Manager, *managerCluster, *environmentGroupVault, Space) {
	t.Helper()
	cluster := newManagerCluster(t, managerClusterOptions{})
	cluster.resources["/apis/external-secrets.io/v1/clustersecretstores/borq-openbao"] = map[string]any{
		"apiVersion": "external-secrets.io/v1", "kind": "ClusterSecretStore", "metadata": map[string]any{"name": "borq-openbao"},
		"spec": map[string]any{"provider": map[string]any{"vault": map[string]any{"server": "http://openbao.borq-system.svc:8200", "path": "secret", "version": "v2", "auth": map[string]any{"tokenSecretRef": map[string]any{"name": "openbao-token", "key": "token", "namespace": "borq-system"}}}}},
	}
	cluster.resources["/api/v1/namespaces/borq-system/secrets/openbao-token"] = map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": "openbao-token", "namespace": "borq-system"}, "data": map[string]any{"token": base64.StdEncoding.EncodeToString([]byte("fixture-token"))}}
	vault := &environmentGroupVault{versions: map[string][]map[string]string{}}
	cluster.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.HasPrefix(request.URL.Path, environmentGroupProxyPrefix()) {
			vault.serve(writer, request)
			return
		}
		cluster.serve(writer, request)
	})
	manager := openTestManager(t, filepath.Join(t.TempDir(), "borq.sqlite"), time.Second)
	environment := createManagerEnvironment(t, manager, cluster, "production")
	return manager, cluster, vault, environment
}

func environmentGroupProxyPrefix() string {
	return "/api/v1/namespaces/borq-system/services/http:openbao:8200/proxy/v1/secret/"
}
func (vault *environmentGroupVault) writes() int {
	vault.mutex.Lock()
	defer vault.mutex.Unlock()
	return vault.writeCount
}
func (vault *environmentGroupVault) retention() int {
	vault.mutex.Lock()
	defer vault.mutex.Unlock()
	return vault.maxVersions
}
func (vault *environmentGroupVault) injectConcurrentWrite() {
	vault.mutex.Lock()
	defer vault.mutex.Unlock()
	vault.concurrentWrite = true
}

func (vault *environmentGroupVault) serve(writer http.ResponseWriter, request *http.Request) {
	vault.mutex.Lock()
	defer vault.mutex.Unlock()
	writer.Header().Set("Content-Type", "application/json")
	path := strings.TrimPrefix(request.URL.Path, environmentGroupProxyPrefix())
	operation, key, _ := strings.Cut(path, "/")
	versions := vault.versions[key]
	if operation == "metadata" {
		if request.Method == http.MethodGet {
			if len(versions) == 0 {
				writer.WriteHeader(404)
				_ = json.NewEncoder(writer).Encode(map[string]any{"errors": []string{"missing"}})
				return
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{"data": map[string]any{"current_version": len(versions), "oldest_version": 1, "max_versions": vault.maxVersions}})
			return
		}
		var input struct {
			MaxVersions int `json:"max_versions"`
		}
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
			http.Error(writer, err.Error(), 400)
			return
		}
		vault.maxVersions = input.MaxVersions
		writer.WriteHeader(204)
		return
	}
	if operation != "data" {
		http.Error(writer, "unsupported fixture endpoint", 404)
		return
	}
	if request.Method == http.MethodGet {
		version := len(versions)
		if text := request.URL.Query().Get("version"); text != "" {
			version, _ = strconv.Atoi(text)
		}
		if version < 1 || version > len(versions) {
			writer.WriteHeader(404)
			_ = json.NewEncoder(writer).Encode(map[string]any{"errors": []string{"missing"}})
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"data": map[string]any{"data": map[string]any{"values": versions[version-1]}, "metadata": map[string]any{"version": version, "deletion_time": "", "destroyed": false}}})
		return
	}
	var input struct {
		Options struct {
			Cas int `json:"cas"`
		} `json:"options"`
		Data struct {
			Values map[string]string `json:"values"`
			PlanID string            `json:"planId"`
		} `json:"data"`
	}
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
		http.Error(writer, err.Error(), 400)
		return
	}
	if vault.concurrentWrite {
		vault.concurrentWrite = false
		versions = append(versions, map[string]string{"CONCURRENT": "preserved"})
		vault.versions[key] = versions
		vault.writeCount++
	}
	if input.Options.Cas != len(versions) {
		writer.WriteHeader(400)
		_ = json.NewEncoder(writer).Encode(map[string]any{"errors": []string{"check-and-set parameter did not match the current version"}})
		return
	}
	vault.versions[key] = append(versions, maps.Clone(input.Data.Values))
	vault.writeCount++
	_ = json.NewEncoder(writer).Encode(map[string]any{"data": map[string]any{"version": len(versions) + 1, "created_time": fmt.Sprint(time.Now().UTC().Format(time.RFC3339Nano)), "deletion_time": "", "destroyed": false}})
}
