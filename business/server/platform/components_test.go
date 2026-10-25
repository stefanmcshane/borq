package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

// TestComponentFailureIsolation verifies an unavailable Helm connection blocks
// consumers, preserves independent attempts, and stops serial upgrades at failure.
// Every attempted or blocked component must report its outcome and setup timing.
// It tests orchestration, not Kubernetes controller readiness or durability.
func TestComponentFailureIsolation(t *testing.T) {
	for _, upgrade := range []bool{false, true} {
		name := "install"
		if upgrade {
			name = "upgrade"
		}
		t.Run(name, func(t *testing.T) {
			is := is.New(t)
			var output bytes.Buffer // The slog handler writes through io.Writer's pointer API.
			previousLogger := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
			t.Cleanup(func() { slog.SetDefault(previousLogger) })
			connection := kubernetes.NewConnectionForTest(fake.NewClientset())
			result, err := reconcileComponents(context.Background(), componentRun{
				connection: connection, scope: "target", upgrade: upgrade,
				target: TargetInput{ID: "cluster-id", Organization: "example", Name: "target"},
				values: valuesInput{Settings: Settings{Profile: "dev", Namespace: "components"}}, definitions: targetCatalog(upgrade),
			})
			is.True(err != nil)
			is.True(strings.Contains(err.Error(), "helm requires a Kubernetes connection"))
			is.Equal(len(result.Components), 5)
			for index, state := range result.Components {
				expected := "failed"
				if state.Name == "alloy" || upgrade && index > 0 {
					expected = "blocked"
				}
				is.Equal(state.Status, expected)
				is.Equal(state.Name, targetCatalog(upgrade)[index].Name)
			}
			started, completed, summaries := 0, map[string]map[string]any{}, 0
			for _, line := range bytes.Split(bytes.TrimSpace(output.Bytes()), []byte("\n")) {
				if len(line) == 0 {
					continue
				}
				var entry map[string]any
				is.NoErr(json.Unmarshal(line, &entry))
				is.Equal(entry["scope"], "target")
				is.Equal(entry["cluster_id"], "cluster-id")
				is.Equal(entry["organization"], "example")
				switch entry["msg"] {
				case "component_setup_started":
					started++
				case "component_setup_finished":
					completed[entry["component"].(string)] = entry
				case "components_setup_finished":
					summaries++
					is.Equal(entry["status"], "failed")
					is.True(entry["duration_ms"].(float64) >= 0)
				}
			}
			is.Equal(len(completed), len(result.Components))
			is.Equal(summaries, 1)
			attempted := 0
			for _, state := range result.Components {
				entry := completed[state.Name]
				is.Equal(entry["status"], state.Status)
				is.True(entry["duration_ms"].(float64) >= 0)
				is.True(entry["wait_duration_ms"].(float64) >= 0)
				is.True(entry["error"] != nil)
				if state.Status == "blocked" {
					is.Equal(entry["duration_ms"], float64(0))
					continue
				}
				attempted++
			}
			is.Equal(started, attempted)
		})
	}
}
