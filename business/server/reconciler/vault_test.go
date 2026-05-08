package reconciler

import (
	"testing"

	"github.com/matryer/is"
)

func TestVaultHandlerName(t *testing.T) {
	is := is.New(t)

	var h VaultHandler
	is.Equal(h.Name(), "vault")
}

func TestVaultHandlerDefaultConfig(t *testing.T) {
	is := is.New(t)

	var h VaultHandler
	cfg := h.DefaultConfig()
	vaultCfg, ok := cfg.(*VaultConfig)
	is.True(ok)
	is.Equal(vaultCfg.Name, "vault")
	is.Equal(vaultCfg.Namespace, "borq-system")
	is.Equal(vaultCfg.Image, "hashicorp/vault:1.21.2")
	is.Equal(vaultCfg.Replicas, int32(1))
	is.Equal(vaultCfg.StorageSize, "10Gi")
}

func TestVaultHandlerPlanWithFullConfig(t *testing.T) {
	is := is.New(t)

	var h VaultHandler
	plan := h.Plan(&VaultConfig{
		Name: "vault", Namespace: "borq-system",
		Image: "hashicorp/vault:1.21.2", Replicas: 1, StorageSize: "10Gi",
	})

	is.Equal(len(plan.Desired), 9)
}

func TestVaultHandlerPlanEmptyConfigDefaults(t *testing.T) {
	is := is.New(t)

	var h VaultHandler
	plan := h.Plan(&VaultConfig{})

	is.Equal(len(plan.Desired), 9)

	var deploy Resource
	for _, res := range plan.Desired {
		if res.Type == TypeDeployment {
			deploy = res
			break
		}
	}

	is.True(deploy.Desired != nil)
	is.Equal(deploy.Name, "vault")
	is.Equal(deploy.Namespace, "borq-system")
}

func TestVaultHandlerPlanCustomConfig(t *testing.T) {
	is := is.New(t)

	var h VaultHandler
	plan := h.Plan(&VaultConfig{
		Name: "prod-vault", Namespace: "prod-system",
		Image: "hashicorp/vault:1.15.0", Replicas: 3, StorageSize: "50Gi",
	})

	is.Equal(len(plan.Desired), 9)

	var foundServiceAccount, foundClusterRole, foundClusterRoleBinding bool
	var foundConfigMap, foundService, foundInternalService bool
	var foundPVC, foundDeploy bool

	for _, res := range plan.Desired {
		switch res.Type {
		case TypeServiceAccount:
			foundServiceAccount = true
			is.Equal(res.Name, "prod-vault")
			is.Equal(res.Namespace, "prod-system")
		case TypeClusterRole:
			foundClusterRole = true
			is.Equal(res.Name, "prod-vault-service-discovery")
		case TypeClusterRoleBinding:
			foundClusterRoleBinding = true
			is.Equal(res.Name, "prod-vault-service-discovery")
		case TypeConfigMap:
			foundConfigMap = true
			is.Equal(res.Name, "prod-vault-config")
			is.Equal(res.Namespace, "prod-system")
		case TypeService:
			if res.Name == "prod-vault" {
				foundService = true
			}
			if res.Name == "prod-vault-internal" {
				foundInternalService = true
			}
		case TypePersistentVolumeClaim:
			foundPVC = true
			is.Equal(res.Name, "prod-vault-data")
		case TypeDeployment:
			foundDeploy = true
			is.Equal(res.Name, "prod-vault")
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

func TestVaultHandlerPlanNegativeReplicasDefaultsToOne(t *testing.T) {
	is := is.New(t)

	var h VaultHandler
	plan := h.Plan(&VaultConfig{
		Name: "vault", Namespace: "borq-system",
		Image: "hashicorp/vault:1.21.2", Replicas: -1, StorageSize: "10Gi",
	})

	var deploy Resource
	for _, res := range plan.Desired {
		if res.Type == TypeDeployment {
			deploy = res
			break
		}
	}

	is.Equal(deploy.Name, "vault")
}

func TestVaultHandlerPlanVaultInitHasNamespace(t *testing.T) {
	is := is.New(t)

	var h VaultHandler
	plan := h.Plan(&VaultConfig{
		Name: "vault", Namespace: "borq-system",
		Image: "hashicorp/vault:1.21.2", Replicas: 1, StorageSize: "10Gi",
	})

	initRes, found := findVaultInit(plan.Desired)
	is.True(found)
	is.Equal(initRes.Namespace, "borq-system")
	is.Equal(initRes.Name, "vault-state")
}

func TestVaultHandlerPlanVaultInitHasCustomNamespace(t *testing.T) {
	is := is.New(t)

	var h VaultHandler
	plan := h.Plan(&VaultConfig{
		Name: "prod-vault", Namespace: "prod-system",
		Image: "hashicorp/vault:1.15.0", Replicas: 3, StorageSize: "50Gi",
	})

	initRes, found := findVaultInit(plan.Desired)
	is.True(found)
	is.Equal(initRes.Namespace, "prod-system")
	is.Equal(initRes.Name, "prod-vault-state")
}

func TestVaultHandlerPlanVaultInitDesiredIsVaultState(t *testing.T) {
	is := is.New(t)

	var h VaultHandler
	plan := h.Plan(&VaultConfig{
		Name: "vault", Namespace: "borq-system",
		Image: "hashicorp/vault:1.21.2", Replicas: 1, StorageSize: "10Gi",
	})

	initRes, found := findVaultInit(plan.Desired)
	is.True(found)

	_, ok := initRes.Desired.(VaultState)
	is.True(ok)
}

func findVaultInit(resources []Resource) (Resource, bool) {
	for _, res := range resources {
		if res.Type == vaultInitType {
			return res, true
		}
	}
	return Resource{}, false
}
