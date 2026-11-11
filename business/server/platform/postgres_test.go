package platform

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/matryer/is"
	"helm.sh/helm/v4/pkg/release/common"
	releasev1 "helm.sh/helm/v4/pkg/release/v1"
)

// TestReadPostgresComponents owns actual Helm readback, expected chart identity,
// partial failures and absence filtering without claiming controller readiness.
func TestReadPostgresComponents(t *testing.T) {
	for _, scenario := range []string{"installed", "absent", "uninstalled", "foreign chart", "partial read"} {
		t.Run(scenario, func(t *testing.T) {
			assert := is.New(t)
			input := diffTestInput()
			input.Settings.Namespace = "borq-system"
			input.Settings.Components = map[string]ComponentOverride{"cloudnative-pg": {Chart: "custom-cnpg", Version: "0.30.0"}}
			settings, err := normalizeSettings(input.Settings)
			assert.NoErr(err)
			var records []releasev1.Release
			for _, definition := range postgresComponents(settings) {
				if scenario == "absent" {
					continue
				}
				record := diffTestRelease(t, definition, input)
				record.Namespace = settings.Namespace
				record.Config["privateKey"] = "private-postgres-value"
				if definition.Name == "cloudnative-pg" {
					record.Chart.Metadata.Version = "0.28.0"
					record.Info.Status = common.StatusFailed
					if scenario == "foreign chart" {
						record.Chart.Metadata.Name = "unrelated-chart"
					}
				} else {
					record.Info.Status = common.StatusPendingUpgrade
				}
				if scenario == "uninstalled" {
					record.Info.Status = common.StatusUninstalled
				}
				records = append(records, record)
			}
			inventory := diffTestInventory{releases: records}
			if scenario == "partial read" {
				inventory.failedRelease = "plugin-barman-cloud"
			}
			path, requests := diffTestServer(t, inventory)
			input.KubeconfigPath = path
			result, err := ReadPostgresComponents(context.Background(), input)
			assert.Equal(err != nil, scenario == "foreign chart" || scenario == "partial read")
			assert.Equal(result.Namespace, "borq-system")
			switch scenario {
			case "absent", "uninstalled":
				assert.Equal(len(result.Components), 0)
			case "foreign chart":
				assert.Equal(len(result.Components), 1)
				assert.Equal(result.Components[0].Name, "plugin-barman-cloud")
			default:
				expected := 2
				if scenario == "partial read" {
					expected = 1
				}
				assert.Equal(len(result.Components), expected)
				cnpg := result.Components[0]
				assert.Equal(cnpg.Name, "cloudnative-pg")
				assert.Equal(cnpg.Chart, "custom-cnpg")
				assert.Equal(cnpg.Version, "0.28.0")
				assert.Equal(cnpg.Revision, 3)
				assert.Equal(cnpg.Status, "failed")
				assert.True(cnpg.UpgradeAvailable)
				if expected == 2 {
					assert.Equal(result.Components[1].Status, "pending-upgrade")
				}
			}
			encoded, err := json.Marshal(result)
			assert.NoErr(err)
			assert.True(!strings.Contains(string(encoded), "private-postgres-value"))
			assert.True(len(requests) > 0)
			for len(requests) > 0 {
				assert.Equal(<-requests, "GET /api/v1/namespaces/borq-system/secrets")
			}
		})
	}
}
