package reconciler

import (
	"testing"

	"github.com/matryer/is"
)

func TestOpenBaoHandlerName(t *testing.T) {
	is := is.New(t)

	var h OpenBaoHandler
	is.Equal(h.Name(), "openbao")
}

func TestOpenBaoHandlerDefaultConfig(t *testing.T) {
	is := is.New(t)

	var h OpenBaoHandler
	cfg := h.DefaultConfig()
	openBaoCfg, ok := cfg.(*OpenBaoConfig)
	is.True(ok)
	is.Equal(openBaoCfg.Name, "openbao")
	is.Equal(openBaoCfg.Namespace, "borq-mgmt")
	is.Equal(openBaoCfg.Image, "ghcr.io/openbao/openbao:2.4.4")
	is.Equal(openBaoCfg.Replicas, int32(1))
	is.Equal(openBaoCfg.StorageSize, "10Gi")
}

func TestOpenBaoHandlerPlanWithFullConfig(t *testing.T) {
	is := is.New(t)

	var h OpenBaoHandler
	plan := h.Plan(&OpenBaoConfig{
		Name: "openbao", Namespace: "borq-mgmt",
		Image: "ghcr.io/openbao/openbao:2.4.4", Replicas: 1, StorageSize: "10Gi",
	})

	is.Equal(len(plan.Desired), 9)
}

func TestOpenBaoHandlerPlanEmptyConfigDefaults(t *testing.T) {
	is := is.New(t)

	var h OpenBaoHandler
	plan := h.Plan(&OpenBaoConfig{})

	is.Equal(len(plan.Desired), 9)

	var deploy Resource
	for _, res := range plan.Desired {
		if res.Type == TypeDeployment {
			deploy = res
			break
		}
	}

	is.True(deploy.Desired != nil)
	is.Equal(deploy.Name, "openbao")
	is.Equal(deploy.Namespace, "borq-mgmt")
}

func TestOpenBaoHandlerPlanCustomConfig(t *testing.T) {
	is := is.New(t)

	var h OpenBaoHandler
	plan := h.Plan(&OpenBaoConfig{
		Name: "prod-openbao", Namespace: "prod-system",
		Image: "ghcr.io/openbao/openbao:2.4.4", Replicas: 3, StorageSize: "50Gi",
	})

	is.Equal(len(plan.Desired), 9)

	var foundServiceAccount, foundClusterRole, foundClusterRoleBinding bool
	var foundConfigMap, foundService, foundInternalService bool
	var foundPVC, foundDeploy bool

	for _, res := range plan.Desired {
		switch res.Type {
		case TypeServiceAccount:
			foundServiceAccount = true
			is.Equal(res.Name, "prod-openbao")
			is.Equal(res.Namespace, "prod-system")
		case TypeClusterRole:
			foundClusterRole = true
			is.Equal(res.Name, "prod-openbao-service-discovery")
		case TypeClusterRoleBinding:
			foundClusterRoleBinding = true
			is.Equal(res.Name, "prod-openbao-service-discovery")
		case TypeConfigMap:
			foundConfigMap = true
			is.Equal(res.Name, "prod-openbao-config")
			is.Equal(res.Namespace, "prod-system")
		case TypeService:
			if res.Name == "prod-openbao" {
				foundService = true
			}
			if res.Name == "prod-openbao-internal" {
				foundInternalService = true
			}
		case TypePersistentVolumeClaim:
			foundPVC = true
			is.Equal(res.Name, "prod-openbao-data")
		case TypeDeployment:
			foundDeploy = true
			is.Equal(res.Name, "prod-openbao")
			is.Equal(res.Namespace, "prod-system")
		}
	}

	is.True(foundServiceAccount)
	is.True(foundClusterRole)
	is.True(foundClusterRoleBinding)
	is.True(foundConfigMap)
	is.True(foundService)
	is.True(foundInternalService)
	is.True(foundPVC)
	is.True(foundDeploy)
}

func TestOpenBaoHandlerPlanNegativeReplicasDefaultsToOne(t *testing.T) {
	is := is.New(t)

	var h OpenBaoHandler
	plan := h.Plan(&OpenBaoConfig{
		Name: "openbao", Namespace: "borq-mgmt",
		Image: "ghcr.io/openbao/openbao:2.4.4", Replicas: -1, StorageSize: "10Gi",
	})

	var deploy Resource
	for _, res := range plan.Desired {
		if res.Type == TypeDeployment {
			deploy = res
			break
		}
	}

	is.Equal(deploy.Name, "openbao")
}

func TestOpenBaoHandlerPlanOpenBaoInitHasNamespace(t *testing.T) {
	is := is.New(t)

	var h OpenBaoHandler
	plan := h.Plan(&OpenBaoConfig{
		Name: "openbao", Namespace: "borq-mgmt",
		Image: "ghcr.io/openbao/openbao:2.4.4", Replicas: 1, StorageSize: "10Gi",
	})

	initRes, found := findOpenBaoInit(plan.Desired)
	is.True(found)
	is.Equal(initRes.Namespace, "borq-mgmt")
	is.Equal(initRes.Name, "openbao-state")
}

func TestOpenBaoHandlerPlanOpenBaoInitHasCustomNamespace(t *testing.T) {
	is := is.New(t)

	var h OpenBaoHandler
	plan := h.Plan(&OpenBaoConfig{
		Name: "prod-openbao", Namespace: "prod-system",
		Image: "ghcr.io/openbao/openbao:2.4.4", Replicas: 3, StorageSize: "50Gi",
	})

	initRes, found := findOpenBaoInit(plan.Desired)
	is.True(found)
	is.Equal(initRes.Namespace, "prod-system")
	is.Equal(initRes.Name, "prod-openbao-state")
}

func TestOpenBaoHandlerPlanOpenBaoInitDesiredIsOpenBaoState(t *testing.T) {
	is := is.New(t)

	var h OpenBaoHandler
	plan := h.Plan(&OpenBaoConfig{
		Name: "openbao", Namespace: "borq-mgmt",
		Image: "ghcr.io/openbao/openbao:2.4.4", Replicas: 1, StorageSize: "10Gi",
	})

	initRes, found := findOpenBaoInit(plan.Desired)
	is.True(found)

	_, ok := initRes.Desired.(OpenBaoState)
	is.True(ok)
}

func findOpenBaoInit(resources []Resource) (Resource, bool) {
	for _, res := range resources {
		if res.Type == openBaoInitType {
			return res, true
		}
	}
	return Resource{}, false
}
