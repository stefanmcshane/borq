package apps

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/helm"
)

// TestAddonKubernetesEventScope owns filtering arbitrary rendered workload names
// and their controller descendants, excluding other kinds, namespaces and addons.
func TestAddonKubernetesEventScope(t *testing.T) {
	assert := is.New(t)
	now := time.Now().UTC()
	resources := []helm.ResourceIdentity{
		{Namespace: "selected", Kind: "Deployment", Name: "custom-api"},
		{Namespace: "selected", Kind: "StatefulSet", Name: "custom-nats"},
		{Namespace: "selected", Kind: "DaemonSet", Name: "agent"},
		{Namespace: "selected", Kind: "Job", Name: "migrate"},
		{Namespace: "selected", Kind: "CronJob", Name: "backup"},
		{Namespace: "other", Kind: "Deployment", Name: "foreign"},
	}
	accepted := []struct{ kind, name string }{
		{"Deployment", "custom-api"}, {"ReplicaSet", "custom-api-7b89c56dd8"}, {"Pod", "custom-api-7b89c56dd8-abc12"},
		{"StatefulSet", "custom-nats"}, {"Pod", "custom-nats-0"}, {"Pod", "agent-abc12"}, {"Pod", "migrate-abc12"},
		{"Job", "backup-29123456"}, {"Pod", "backup-29123456-abc12"},
	}
	rejected := []struct{ kind, name string }{
		{"Pod", "custom-api-other-abc12"}, {"Deployment", "custom-api-other"}, {"Pod", "foreign-7b89c56dd8-abc12"},
		{"Pod", "custom-nats-extra-0"}, {"Deployment", "custom-nats-0"}, {"Pod", "backup-other-abc12"},
	}
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		query := request.URL.Query().Get("query")
		assert.True(strings.Contains(query, `borq_cluster="cluster-id",namespace="selected",job="borq-kubernetes-events"`))
		assert.Equal(request.URL.Query().Get("limit"), "500")
		_, encoded, ok := strings.Cut(query, "| name=~")
		assert.True(ok)
		pattern, err := strconv.Unquote(encoded)
		assert.NoErr(err)
		matcher, err := regexp.Compile("^(?:" + pattern + ")$")
		assert.NoErr(err)
		for _, object := range accepted {
			assert.True(matcher.MatchString(object.name))
		}
		assert.True(!matcher.MatchString("foreign-7b89c56dd8-abc12"))
		assert.True(!matcher.MatchString("custom-api-other"))
		values := [][2]string{}
		for _, object := range append(accepted, rejected...) {
			encoded, err := json.Marshal(map[string]any{"kind": object.kind, "name": object.name, "reason": "Started", "count": 2, "msg": "Started container"})
			assert.NoErr(err)
			values = append(values, [2]string{strconv.FormatInt(now.UnixNano(), 10), string(encoded)})
		}
		assert.NoErr(json.NewEncoder(writer).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "streams", "result": []any{map[string]any{"stream": map[string]string{}, "values": values}}}}))
	}))
	t.Cleanup(backend.Close)
	manager, err := NewManager(context.Background(), NewInput{DatabasePath: filepath.Join(t.TempDir(), "events.sqlite"), LokiQueryUrl: backend.URL})
	assert.NoErr(err)
	t.Cleanup(func() { assert.NoErr(manager.Close()) })
	events, notice := manager.kubernetesEvents(context.Background(), "cluster-id", "selected", addonEventObjects(resources, "selected"), now.Add(-time.Hour), now.Add(time.Second))
	assert.Equal(notice, "")
	assert.Equal(len(events), len(accepted))
	restarts := 0
	for _, event := range events {
		if event.Kind == "restart" {
			restarts++
		}
	}
	assert.Equal(restarts, 5)
}
