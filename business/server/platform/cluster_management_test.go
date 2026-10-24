package platform

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/matryer/is"
	releasev1 "helm.sh/helm/v4/pkg/release/v1"
)

// TestReadClusterManagement observes real Helm Secret encoding through the
// selected kubecontext, omits absent releases and never writes to the cluster.
func TestReadClusterManagement(t *testing.T) {
	for _, installed := range []bool{false, true} {
		t.Run(map[bool]string{false: "no management services", true: "installed management services"}[installed], func(t *testing.T) {
			assert := is.New(t)
			input := diffTestInput()
			settings, err := normalizeSettings(input.Settings)
			assert.NoErr(err)
			var records []releasev1.Release
			if installed {
				for _, definition := range managementCatalog(settings, false) {
					if definition.Name != "prometheus" {
						continue
					}
					record := diffTestRelease(t, definition, input)
					record.Config["privateKey"] = "private-component-value"
					records = append(records, record)
				}
			}
			path, requests := diffTestServer(t, diffTestInventory{releases: records})
			input.KubeconfigPath = path
			result, err := ReadClusterManagement(context.Background(), input)
			assert.NoErr(err)
			if installed {
				assert.Equal(len(result.Components), 1)
				assert.Equal(result.Components[0].Name, "prometheus")
				assert.Equal(result.Components[0].Revision, 3)
				assert.Equal(result.Components[0].Status, "deployed")
			} else {
				assert.Equal(len(result.Components), 0)
			}
			encoded, err := json.Marshal(result)
			assert.NoErr(err)
			assert.True(!strings.Contains(string(encoded), "private-component-value"))
			assert.True(len(requests) > 0)
			for len(requests) > 0 {
				assert.Equal(<-requests, "GET /api/v1/namespaces/borq/secrets")
			}
		})
	}
}
