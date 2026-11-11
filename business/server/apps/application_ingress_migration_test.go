package apps

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/sqlite"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// This fixture preserves the Ingress shape emitted before Gateway API rendering.
func legacyApplicationIngress(namespace, target string) networkingv1.Ingress {
	class, prefix := "traefik", networkingv1.PathTypePrefix
	return networkingv1.Ingress{
		TypeMeta: metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "Ingress"},
		ObjectMeta: metav1.ObjectMeta{Name: "web-public", Namespace: namespace, Labels: map[string]string{
			"app.kubernetes.io/name": "web", "app.kubernetes.io/managed-by": "borq", "borq.run/target-id": target,
		}},
		Spec: networkingv1.IngressSpec{IngressClassName: &class,
			Rules: []networkingv1.IngressRule{{Host: "web.example.com", IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{Paths: []networkingv1.HTTPIngressPath{{Path: "/", PathType: &prefix, Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: "web", Port: networkingv1.ServiceBackendPort{Number: 80}}}}}}}}},
			TLS:   []networkingv1.IngressTLS{{Hosts: []string{"web.example.com"}, SecretName: "web-public-tls"}},
		},
	}
}

func TestApplicationIngressMigrationPlan(t *testing.T) {
	assert := is.New(t)
	cluster := newManagerCluster(t, managerClusterOptions{})
	databasePath := filepath.Join(t.TempDir(), "state.sqlite")
	manager := openTestManager(t, databasePath, time.Second)
	space := createManagerEnvironment(t, manager, cluster, "production")
	configuration, err := Merge(Configuration{}, patchFromJson(t, applicationRouteTestPatch("web.example.com")))
	assert.NoErr(err)
	rendered, err := RenderForPlan(configuration, space.Namespace, space.ID, "old", space.CertificateIssuer)
	assert.NoErr(err)
	manifests := []map[string]any{findManifest(t, rendered, "Deployment", "web"), findManifest(t, rendered, "Service", "web")}
	ingress := legacyApplicationIngress(space.Namespace, space.ID)
	ingress.UID, ingress.ResourceVersion = "legacy-ingress", "1"
	manifests = append(manifests, applicationTunnelManifest(t, ingress))
	cluster.store(manifests[2])
	stored, record, err := loadDocument[spaceRecord](t.Context(), manager.database, "space", spaceKey("acme", "production"))
	assert.NoErr(err)
	stored.Space.Configuration, stored.Manifests = configuration, manifests
	write, err := documentWrite("space", spaceKey("acme", "production"), stored, record.Revision)
	assert.NoErr(err)
	assert.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{write}))
	plan, err := manager.Plan(t.Context(), managerPlanInput(t, "production", `{}`))
	if err != nil {
		t.Fatalf("unchanged public application cannot plan its Gateway migration: %v", err)
	}
	findManifest(t, Rendered{Manifests: plan.Manifests}, "HTTPRoute", "web-public")
	assert.Equal(plan.ResourceDiffs[len(plan.ResourceDiffs)-1].Action, "delete")
	_, err = manager.Plan(t.Context(), managerPlanInput(t, "production", `{"apps":[{"name":"web","ingresses":[]}]}`))
	assert.True(err != nil && strings.Contains(err.Error(), "resource deletion is deferred"))
	// A failed accepted rollout has Gateway desired state but still owns the
	// live old Ingress. Reopening must preserve cleanup in the next plan.
	saved, _, err := loadDocument[planRecord](t.Context(), manager.database, "plan", plan.ID)
	assert.NoErr(err)
	assert.Equal(len(saved.RetiredIngresses), 1)
	assert.Equal(manifestText(saved.RetiredIngresses[0], "metadata", "uid"), "legacy-ingress")
	stored, record, err = loadDocument[spaceRecord](t.Context(), manager.database, "space", spaceKey("acme", "production"))
	assert.NoErr(err)
	stored.Manifests, stored.LatestOperation = plan.Manifests, plan.ID
	write, err = documentWrite("space", spaceKey("acme", "production"), stored, record.Revision)
	assert.NoErr(err)
	operationWrite, err := documentWrite("operation", plan.ID, Operation{ID: plan.ID, PlanID: plan.ID, SpaceID: space.ID, Status: "failed"}, 0)
	assert.NoErr(err)
	assert.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{write, operationWrite}))
	assert.NoErr(manager.Close())
	manager = openTestManager(t, databasePath, time.Second)
	retried, err := manager.Plan(t.Context(), managerPlanInput(t, "production", `{}`))
	assert.NoErr(err)
	retriedRecord, _, err := loadDocument[planRecord](t.Context(), manager.database, "plan", retried.ID)
	assert.NoErr(err)
	assert.Equal(retriedRecord.RetiredIngresses, saved.RetiredIngresses)
	inventory, err := manager.appDeletionInventory(t.Context(), stored, "web")
	assert.NoErr(err)
	findManifest(t, Rendered{Manifests: inventory}, "Ingress", "web-public")
	ingress.UID = "replacement-ingress"
	cluster.store(applicationTunnelManifest(t, ingress))
	_, err = manager.Plan(t.Context(), managerPlanInput(t, "production", `{}`))
	assert.True(err != nil && strings.Contains(err.Error(), "outside the saved application"))
}

// Accepted configuration alone does not prove that the new dataplane is ready.
func TestApplicationIngressRetirementRequiresProgrammedGateway(t *testing.T) {
	for _, condition := range []struct {
		name       string
		conditions []any
	}{
		{name: "missing"},
		{name: "pending", conditions: []any{map[string]any{"type": "Programmed", "status": "False", "observedGeneration": float64(2)}}},
		{name: "stale", conditions: []any{map[string]any{"type": "Programmed", "status": "True", "observedGeneration": float64(1)}}},
	} {
		t.Run(condition.name, func(t *testing.T) {
			assert := is.New(t)
			cluster := newManagerCluster(t, managerClusterOptions{})
			manager := openTestManager(t, filepath.Join(t.TempDir(), "state.sqlite"), time.Second)
			configuration, err := Merge(Configuration{}, patchFromJson(t, applicationRouteTestPatch("web.example.com")))
			assert.NoErr(err)
			rendered, err := RenderForPlan(configuration, "apps", "space-one", "migration", "issuer")
			assert.NoErr(err)
			gateway := findManifest(t, rendered, "Gateway", "web-public")
			gateway["metadata"].(map[string]any)["generation"] = float64(2)
			gateway["status"] = map[string]any{"conditions": condition.conditions}
			cluster.store(gateway)
			ingress := applicationTunnelManifest(t, legacyApplicationIngress("apps", "space-one"))
			cluster.store(ingress)
			err = manager.retireApplicationIngresses(t.Context(), planRecord{Plan: Plan{Space: Space{ID: "space-one"}, Manifests: rendered.Manifests}, Connection: Cluster{KubeconfigPath: cluster.kubeconfig}, RetiredIngresses: []map[string]any{ingress}})
			if err == nil || !strings.Contains(err.Error(), "replacement Gateway is programmed") {
				t.Fatalf("expected refusal for %s Programmed condition, got %v", condition.name, err)
			}
		})
	}
}
