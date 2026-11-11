package reconciler

import (
	"testing"

	"github.com/matryer/is"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
	is.True(namesByType[secretStoreType]["openbao-backend"])
	is.True(namesByType[pushSecretType]["openbao-bootstrap"])
	namespaceIndex := indexOf(plan.ApplyOrder, "Namespace")
	serviceAccountIndex := indexOf(plan.ApplyOrder, "ServiceAccount")
	deploymentIndex := indexOf(plan.ApplyOrder, "Deployment.apps")

	is.True(namespaceIndex >= 0)
	is.True(serviceAccountIndex >= 0)
	is.True(deploymentIndex >= 0)
	is.True(namespaceIndex < serviceAccountIndex)
	is.True(namespaceIndex < deploymentIndex)
}

// TestExternalSecretsPlanPushesOpenBaoBootstrap verifies the ESO plan
// includes a PushSecret that mirrors the OpenBao bootstrap secret into Vault.
func TestExternalSecretsPlanPushesOpenBaoBootstrap(t *testing.T) {
	is := is.New(t)

	plan := ExternalSecretsHandler{}.Plan(&ExternalSecretsConfig{Namespace: "custom-secrets"})

	var pushSecret *unstructured.Unstructured
	for _, res := range plan.Desired {
		if res.Type != pushSecretType {
			continue
		}
		var ok bool
		pushSecret, ok = res.Desired.(*unstructured.Unstructured)
		is.True(ok)
		break
	}

	if pushSecret == nil {
		t.Fatal("expected pushsecret in desired resources")
	}

	is.Equal(pushSecret.GetNamespace(), "custom-secrets")
	dataTo, found, err := unstructured.NestedSlice(pushSecret.Object, "spec", "dataTo")
	is.NoErr(err)
	is.True(found)
	is.Equal(len(dataTo), 1)

	firstEntry, ok := dataTo[0].(map[string]any)
	is.True(ok)
	is.Equal(firstEntry["remoteKey"], "custom-secrets/openbao-unseal")
}

// TestExternalSecretsRoleIncludesGeneratorPermissions verifies the
// handcrafted ESO RBAC covers the generators API group used by the controller.
func TestExternalSecretsRoleIncludesGeneratorPermissions(t *testing.T) {
	is := is.New(t)

	role := externalSecretsRole(ExternalSecretsConfig{Namespace: "borq-mgmt"})
	is.Equal(role.Kind, "Role")
	is.Equal(role.Namespace, "borq-mgmt")

	var generatorReadRule *rbacv1.PolicyRule
	var generatorStateRule *rbacv1.PolicyRule
	for i := range role.Rules {
		rule := &role.Rules[i]
		if len(rule.APIGroups) != 1 || rule.APIGroups[0] != "generators.external-secrets.io" {
			continue
		}
		if contains(rule.Resources, "generatorstates") {
			generatorStateRule = rule
			continue
		}
		if contains(rule.Resources, "vaultdynamicsecrets") {
			generatorReadRule = rule
		}
	}

	is.True(generatorReadRule != nil)
	is.True(contains(generatorReadRule.Verbs, "list"))
	is.True(contains(generatorReadRule.Verbs, "watch"))
	is.True(generatorStateRule != nil)
	is.True(contains(generatorStateRule.Verbs, "create"))
	is.True(contains(generatorStateRule.Verbs, "patch"))
	is.True(contains(generatorStateRule.Verbs, "deletecollection"))
}

// TestExternalSecretsRoleMatchesLatestUpstreamShape verifies the
// handcrafted controller RBAC keeps the key permission splits used by the
// latest upstream ESO chart controller role.
func TestExternalSecretsRoleMatchesLatestUpstreamShape(t *testing.T) {
	is := is.New(t)

	role := externalSecretsRole(ExternalSecretsConfig{Namespace: "borq-mgmt"})

	is.True(hasRule(role.Rules, []string{""}, []string{"serviceaccounts/token"}, []string{"create"}))
	is.True(hasRule(role.Rules, []string{""}, []string{"events"}, []string{"create", "patch"}))
	is.True(hasRule(role.Rules, []string{"coordination.k8s.io"}, []string{"leases"}, []string{"get", "create", "update", "patch"}))
	is.True(!hasResource(role.Rules, []string{""}, "pods"))
	is.True(!hasResource(role.Rules, []string{""}, "services"))
	is.True(!hasResource(role.Rules, []string{""}, "namespaces"))
	is.True(!hasResource(role.Rules, []string{"external-secrets.io"}, "clustersecretstores"))
}

func contains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func hasRule(rules []rbacv1.PolicyRule, apiGroups []string, resources []string, verbs []string) bool {
	for _, rule := range rules {
		if !sameMembers(rule.APIGroups, apiGroups) {
			continue
		}
		if !sameMembers(rule.Resources, resources) {
			continue
		}
		if !sameMembers(rule.Verbs, verbs) {
			continue
		}
		return true
	}
	return false
}

func hasResource(rules []rbacv1.PolicyRule, apiGroups []string, resource string) bool {
	for _, rule := range rules {
		if !sameMembers(rule.APIGroups, apiGroups) {
			continue
		}
		if contains(rule.Resources, resource) {
			return true
		}
	}
	return false
}

func sameMembers(left []string, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for _, value := range left {
		if !contains(right, value) {
			return false
		}
	}
	return true
}
