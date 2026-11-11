package platform

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/matryer/is"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

func pocketIdLegacyFixture(t *testing.T, input PocketIdInput) (appsv1.Deployment, string) {
	t.Helper()
	is := is.New(t)
	// Captured before removing the pinned 2b5a37d chart adapter. Keeping its
	// rendered workload and immutable bytes makes migration proof independent
	// of the new native builder, without retaining a chart runtime dependency.
	encoded, err := os.ReadFile("testdata/pocketid-legacy.json")
	is.NoErr(err)
	contents := strings.NewReplacer("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", input.InstanceID,
		"borq-mgmt", input.Namespace, "https://id.example.test", input.IssuerURL).Replace(string(encoded))
	var fixture struct {
		Deployment          appsv1.Deployment `json:"deployment"`
		BackupConfiguration string            `json:"backupConfiguration"`
	}
	is.NoErr(json.Unmarshal([]byte(contents), &fixture))
	return fixture.Deployment, fixture.BackupConfiguration
}

// TestPocketIdLegacyStorageCompatibility preserves resource selectors while
// refusing to reuse the previous plaintext archive configuration.
func TestPocketIdLegacyStorageCompatibility(t *testing.T) {
	is := is.New(t)
	input, _, err := normalizePocketId(pocketIdTestInput(t))
	is.NoErr(err)
	legacy, configuration := pocketIdLegacyFixture(t, input)
	resources, err := pocketIdPrivateResources(input)
	is.NoErr(err)
	is.True(resources[6].(*corev1.ConfigMap).Data["litestream.yml"] != configuration) // Plaintext archive configuration must never be reused.
	is.Equal(resources[2].(*appsv1.Deployment).Spec.Selector, legacy.Spec.Selector)
}
