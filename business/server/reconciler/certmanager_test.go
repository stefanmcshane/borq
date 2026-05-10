package reconciler

import (
	"testing"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	"github.com/matryer/is"
)

func TestCertManagerHandlerName(t *testing.T) {
	is := is.New(t)
	is.Equal(CertManagerHandler{}.Name(), "cert-manager")
}

func TestCertManagerHandlerDefaultConfig(t *testing.T) {
	is := is.New(t)

	cfg, ok := CertManagerHandler{}.DefaultConfig().(*CertManagerConfig)
	is.True(ok)
	is.Equal(cfg.Version, "v1.20.2")
	is.Equal(cfg.Namespace, "borq-system")
	is.Equal(cfg.Image, "quay.io/jetstack/cert-manager-controller:v1.20.2")
}

func TestCertManagerHandlerPlanIncludesCRDs(t *testing.T) {
	is := is.New(t)

	plan := CertManagerHandler{}.Plan(&CertManagerConfig{})
	hasCRDs := false
	for _, res := range plan.Desired {
		if res.Type == "CustomResourceDefinition.apiextensions.k8s.io" {
			hasCRDs = true
			break
		}
	}
	is.True(hasCRDs)
}

func TestCertManagerHandlerPlanIncludesStandardResources(t *testing.T) {
	is := is.New(t)

	plan := CertManagerHandler{}.Plan(&CertManagerConfig{})
	namesByType := map[string]map[string]bool{}
	for _, res := range plan.Desired {
		if namesByType[res.Type] == nil {
			namesByType[res.Type] = map[string]bool{}
		}
		namesByType[res.Type][res.Name] = true
	}

	is.True(namesByType["Namespace"]["borq-system"])
	is.True(namesByType["ServiceAccount"]["cert-manager"])
	is.True(namesByType["ClusterRole.rbac.authorization.k8s.io"]["cert-manager"])
	is.True(namesByType["ClusterRoleBinding.rbac.authorization.k8s.io"]["cert-manager"])
	is.True(namesByType["Deployment.apps"]["cert-manager"])
}

func TestCertManagerHandlerPlanIncludesCertManagerTypes(t *testing.T) {
	is := is.New(t)

	plan := CertManagerHandler{}.Plan(&CertManagerConfig{})
	namesByType := map[string]map[string]bool{}
	for _, res := range plan.Desired {
		if namesByType[res.Type] == nil {
			namesByType[res.Type] = map[string]bool{}
		}
		namesByType[res.Type][res.Name] = true
	}

	is.True(namesByType["ClusterIssuer.cert-manager.io"]["borq-selfsigned-issuer"])
}

func TestCertManagerHandlerPlanIncludesReadyCheck(t *testing.T) {
	is := is.New(t)

	plan := CertManagerHandler{}.Plan(&CertManagerConfig{})
	hasReady := false
	for _, res := range plan.Desired {
		if res.Type == certManagerReadyType {
			hasReady = true
			is.Equal(res.Name, certManagerDeploymentName)
			break
		}
	}
	is.True(hasReady)
}

func TestCertManagerHandlerPlanClusterIssuerIsSelfSigned(t *testing.T) {
	is := is.New(t)

	plan := CertManagerHandler{}.Plan(&CertManagerConfig{})
	var clusterIssuer *cmapi.ClusterIssuer
	for _, res := range plan.Desired {
		if res.Type == certManagerClusterIssuerType {
			clusterIssuer = res.Desired.(*cmapi.ClusterIssuer)
			break
		}
	}
	is.True(clusterIssuer != nil)
	is.Equal(clusterIssuer.Name, "borq-selfsigned-issuer")
	is.True(clusterIssuer.Spec.SelfSigned != nil)
}

func TestCertManagerHandlerReadinessRunsAfterDeployment(t *testing.T) {
	is := is.New(t)

	plan := CertManagerHandler{}.Plan(&CertManagerConfig{})
	deployIndex := indexOf(plan.ApplyOrder, "Deployment.apps")
	readyIndex := indexOf(plan.ApplyOrder, certManagerReadyType)

	is.True(deployIndex >= 0)
	is.True(readyIndex >= 0)
	is.True(readyIndex > deployIndex)
}

func TestCertManagerHandlerPlanEmptyConfigDefaults(t *testing.T) {
	is := is.New(t)

	plan := CertManagerHandler{}.Plan(&CertManagerConfig{})
	is.True(len(plan.Desired) > 0)

	var deploy Resource
	for _, res := range plan.Desired {
		if res.Type == "Deployment.apps" {
			deploy = res
			break
		}
	}
	is.True(deploy.Desired != nil)
	is.Equal(deploy.Name, "cert-manager")
	is.Equal(deploy.Namespace, "borq-system")
}
