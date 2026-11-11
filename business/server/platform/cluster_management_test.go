package platform

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/matryer/is"
	releasev1 "helm.sh/helm/v4/pkg/release/v1"
	"k8s.io/client-go/tools/clientcmd"
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
			// Application clusters also have these releases; they must not make the
			// selected cluster appear to host Borq's management services.
			for _, definition := range sharedCatalog() {
				records = append(records, diffTestRelease(t, definition, input))
			}
			if installed {
				for _, definition := range managementCatalog(settings, false) {
					if definition.Name != "prometheus" {
						continue
					}
					record := diffTestRelease(t, definition, input)
					record.Namespace = ManagementNamespace
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
				assert.Equal(<-requests, "GET /api/v1/namespaces/borq-mgmt/secrets")
			}
		})
	}
}

// TestReadManagementSharedReleases reads actual Helm records in borq-system even
// though the management installation's state remains in borq-mgmt.
func TestReadManagementSharedReleases(t *testing.T) {
	assert := is.New(t)
	input := diffTestInput()
	var records []releasev1.Release
	for _, definition := range sharedCatalog() {
		records = append(records, diffTestRelease(t, definition, input))
	}
	path, requests := diffTestServer(t, diffTestInventory{releases: records})
	configuration, err := clientcmd.LoadFromFile(path)
	assert.NoErr(err)
	configuration.CurrentContext = "selected"
	assert.NoErr(clientcmd.WriteToFile(*configuration, path))
	result, err := ReadManagement(context.Background(), ManagementInput{KubeconfigPath: path})
	assert.NoErr(err)
	assert.Equal(result.Namespace, "borq-mgmt")
	observed := map[string]string{}
	for _, state := range result.Components {
		if state.Status == "deployed" {
			observed[state.Name] = state.Namespace
			assert.Equal(state.Revision, 3)
			assert.True(!state.UpgradeAvailable)
		}
	}
	assert.Equal(observed, map[string]string{"cert-manager": "borq-system", "traefik": "borq-system"})
	for len(requests) > 0 {
		assert.True(strings.HasPrefix(<-requests, "GET "))
	}
}
