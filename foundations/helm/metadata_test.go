package helm

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/matryer/is"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// TestChartCollectionMetadata covers native workload templates and preserves chart selectors,
// ensuring collection is independent of chart-specific podLabels value conventions.
func TestChartCollectionMetadata(t *testing.T) {
	for _, input := range []struct {
		kind string
		path []string
	}{
		{"Pod", []string{"metadata", "labels"}},
		{"Deployment", []string{"spec", "template", "metadata", "labels"}},
		{"StatefulSet", []string{"spec", "template", "metadata", "labels"}},
		{"DaemonSet", []string{"spec", "template", "metadata", "labels"}},
		{"Job", []string{"spec", "template", "metadata", "labels"}},
		{"CronJob", []string{"spec", "jobTemplate", "spec", "template", "metadata", "labels"}},
	} {
		t.Run(input.kind, func(t *testing.T) {
			assert := is.New(t)
			for _, selected := range []bool{false, true} {
				role := "worker"
				if selected {
					role = "api"
				}
				document := map[string]any{"apiVersion": "v1", "kind": input.kind, "metadata": map[string]any{"name": "upstream"}}
				assert.NoErr(unstructured.SetNestedStringMap(document, map[string]string{"role": role, "upstream": "retained"}, input.path...))
				assert.NoErr(unstructured.SetNestedStringMap(document, map[string]string{"role": role}, "spec", "selector", "matchLabels"))
				source, err := json.Marshal(document)
				assert.NoErr(err)
				renderer := metadataRenderer{input: InstallOrUpgradeInput{ResourceLabels: map[string]string{"borq.run/addon-name": "test"}, PodLabels: map[string]string{"borq.run/logs": "true", "borq.run/metrics": "true"}, PodSelector: map[string]string{"role": "api"}}}
				output, err := renderer.Run(bytes.NewBuffer(source))
				assert.NoErr(err)
				var result map[string]any
				assert.NoErr(yaml.Unmarshal(output.Bytes(), &result))
				labels, _, err := unstructured.NestedStringMap(result, input.path...)
				assert.NoErr(err)
				assert.Equal(labels["upstream"], "retained")
				assert.Equal(labels["borq.run/addon-name"], "test")
				assert.Equal(labels["borq.run/logs"] == "true", selected)
				assert.Equal(labels["borq.run/metrics"] == "true", selected)
				selector, _, err := unstructured.NestedStringMap(result, "spec", "selector", "matchLabels")
				assert.NoErr(err)
				assert.Equal(selector, map[string]string{"role": role})
			}
		})
	}
}
