package reconciler

import (
	"testing"

	"github.com/matryer/is"
)

// TestExternalSecretsPlanIncludesNamespace verifies the ESO plan creates the
// configured namespace before namespaced workloads.
func TestExternalSecretsPlanIncludesNamespace(t *testing.T) {
	is := is.New(t)

	plan := ExternalSecretsHandler{}.Plan(&ExternalSecretsConfig{Namespace: "custom-secrets"})
	namesByType := map[string]map[string]bool{}
	for _, res := range plan.Desired {
		if namesByType[res.Type] == nil {
			namesByType[res.Type] = map[string]bool{}
		}
		namesByType[res.Type][res.Name] = true
	}

	is.True(namesByType["Namespace"]["custom-secrets"])
	namespaceIndex := indexOf(plan.ApplyOrder, "Namespace")
	serviceAccountIndex := indexOf(plan.ApplyOrder, "ServiceAccount")
	deploymentIndex := indexOf(plan.ApplyOrder, "Deployment.apps")

	is.True(namespaceIndex >= 0)
	is.True(serviceAccountIndex >= 0)
	is.True(deploymentIndex >= 0)
	is.True(namespaceIndex < serviceAccountIndex)
	is.True(namespaceIndex < deploymentIndex)
}
