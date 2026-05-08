package reconciler

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	"github.com/stefanmcshane/borq/foundations/vault"
)

const vaultInitType = "VaultInit"

// VaultConfig describes a vault server to reconcile.
type VaultConfig struct {
	Name        string `json:"name"`
	Namespace   string `json:"namespace"`
	Image       string `json:"image"`
	Replicas    int32  `json:"replicas"`
	StorageSize string `json:"storageSize"`
}

// VaultState captures the live operational state of a vault instance.
type VaultState struct {
	Initialized bool
	Sealed      bool
}

// VaultHandler implements ComponentHandler for the vault component.
type VaultHandler struct {
	CoreApplier
	Client *vault.Client
	Log    logger.Logger
}

func (VaultHandler) Name() string { return "vault" }

func (VaultHandler) DefaultConfig() any {
	return &VaultConfig{
		Name: "vault", Namespace: "borq-system",
		Image: "hashicorp/vault:1.21.2", Replicas: 1, StorageSize: "10Gi",
	}
}

func (vh VaultHandler) Plan(config any) Plan {
	cfg := config.(*VaultConfig)
	if cfg.Name == "" {
		cfg.Name = "vault"
	}
	if cfg.Namespace == "" {
		cfg.Namespace = "borq-system"
	}
	if cfg.Image == "" {
		cfg.Image = "hashicorp/vault:1.21.2"
	}
	if cfg.Replicas <= 0 {
		cfg.Replicas = 1
	}
	if cfg.StorageSize == "" {
		cfg.StorageSize = "10Gi"
	}

	labels := vaultComponentLabels
	selector := map[string]string{"app.kubernetes.io/name": cfg.Name}
	podLabels := map[string]string{
		"app.kubernetes.io/name":       cfg.Name,
		"app.kubernetes.io/managed-by": "borq",
		"app.kubernetes.io/component":  "vault",
	}

	plan := Plan{
		ApplyOrder: vaultApplyOrder,
		Desired:    vaultResources(*cfg, labels, selector, podLabels),
		Current:    vaultCurrentFunc(vh.Log, vh.Client, *cfg, labels, selector),
	}

	return plan
}

func vaultCurrentFunc(log logger.Logger, vc *vault.Client, cfg VaultConfig, componentLabels, selector map[string]string) func(ctx context.Context, conn kubernetes.Connection) ([]Resource, error) {
	return func(ctx context.Context, conn kubernetes.Connection) ([]Resource, error) {
		var resources []Resource

		k8sResources, err := listVaultK8sResources(ctx, conn, cfg.Namespace, componentLabels, selector)
		if err != nil {
			return nil, err
		}
		resources = append(resources, k8sResources...)

		log.Debug("vault_current_discover_state")
		vaultState, err := discoverVaultState(ctx, vc)
		if err != nil {
			log.Debug("vault_current_state_unreachable", logger.WithFields(
				logger.String("error", err.Error()),
			))
			resources = append(resources, Resource{
				Type: vaultInitType, Name: cfg.Name + "-state",
				Actual: VaultState{Initialized: false, Sealed: true},
			})
			return resources, nil
		}
		log.Debug("vault_current_state", logger.WithFields(
			logger.Bool("initialized", vaultState.Initialized),
			logger.Bool("sealed", vaultState.Sealed),
		))
		resources = append(resources, Resource{
			Type: vaultInitType, Name: cfg.Name + "-state",
			Actual: vaultState,
		})
		return resources, nil
	}
}

func listVaultK8sResources(ctx context.Context, conn kubernetes.Connection, namespace string, componentLabels, selector map[string]string) ([]Resource, error) {
	labelSelector := "app.kubernetes.io/managed-by=borq,app.kubernetes.io/component=vault"
	var resources []Resource

	svcs, err := conn.Services().List(ctx, namespace, labelSelector)
	if err != nil {
		return nil, fmt.Errorf("list vault services: %w", err)
	}
	for _, s := range svcs {
		resources = append(resources, Resource{Type: TypeService, Namespace: namespace, Name: s.Name, Actual: s})
	}

	cms, err := conn.ConfigMaps().List(ctx, namespace, labelSelector)
	if err != nil {
		return nil, fmt.Errorf("list vault configmaps: %w", err)
	}
	for _, cm := range cms {
		resources = append(resources, Resource{Type: TypeConfigMap, Namespace: namespace, Name: cm.Name, Actual: cm})
	}

	sas, err := conn.ServiceAccounts().List(ctx, namespace, labelSelector)
	if err != nil {
		return nil, fmt.Errorf("list vault serviceaccounts: %w", err)
	}
	for _, sa := range sas {
		resources = append(resources, Resource{Type: TypeServiceAccount, Namespace: namespace, Name: sa.Name, Actual: sa})
	}

	pvcs, err := conn.PersistentVolumeClaims().List(ctx, namespace, labelSelector)
	if err != nil {
		return nil, fmt.Errorf("list vault pvcs: %w", err)
	}
	for _, pvc := range pvcs {
		resources = append(resources, Resource{Type: TypePersistentVolumeClaim, Namespace: namespace, Name: pvc.Name, Actual: pvc})
	}

	deploys, err := conn.Deployments().List(ctx, namespace, labelSelector)
	if err != nil {
		return nil, fmt.Errorf("list vault deployments: %w", err)
	}
	for _, d := range deploys {
		resources = append(resources, Resource{Type: TypeDeployment, Namespace: namespace, Name: d.Name, Actual: d})
	}

	secrets, err := conn.Secrets().List(ctx, namespace, labelSelector)
	if err != nil {
		return nil, fmt.Errorf("list vault secrets: %w", err)
	}
	for _, sec := range secrets {
		resources = append(resources, Resource{Type: TypeSecret, Namespace: namespace, Name: sec.Name, Actual: sec})
	}

	crs, err := conn.ClusterRoles().List(ctx, labelSelector)
	if err != nil {
		return nil, fmt.Errorf("list vault clusterroles: %w", err)
	}
	for _, cr := range crs {
		resources = append(resources, Resource{Type: TypeClusterRole, Name: cr.Name, Actual: cr})
	}

	crbs, err := conn.ClusterRoleBindings().List(ctx, labelSelector)
	if err != nil {
		return nil, fmt.Errorf("list vault clusterrolebindings: %w", err)
	}
	for _, crb := range crbs {
		resources = append(resources, Resource{Type: TypeClusterRoleBinding, Name: crb.Name, Actual: crb})
	}

	return resources, nil
}

func discoverVaultState(ctx context.Context, vc *vault.Client) (VaultState, error) {
	status, err := vc.Status(ctx)
	if err != nil {
		return VaultState{}, err
	}
	return VaultState{Initialized: status.Initialized, Sealed: status.Sealed}, nil
}

func vaultResources(cfg VaultConfig, labels, selector, podLabels map[string]string) []Resource {
	return append(
		vaultK8sResources(cfg, labels, selector, podLabels),
		Resource{
			Type:      vaultInitType,
			Namespace: cfg.Namespace,
			Name:      cfg.Name + "-state",
			Desired:   VaultState{Initialized: true, Sealed: false},
		},
	)
}

func vaultK8sResources(cfg VaultConfig, labels, selector, podLabels map[string]string) []Resource {
	return []Resource{
		{
			Type: TypeServiceAccount, Namespace: cfg.Namespace, Name: cfg.Name,
			Desired: kubernetes.ServiceAccount{
				Name: cfg.Name, Namespace: cfg.Namespace, Labels: labels,
			},
		},
		{
			Type: TypeClusterRole, Name: cfg.Name + "-service-discovery",
			Desired: kubernetes.ClusterRole{
				Name:   cfg.Name + "-service-discovery",
				Labels: labels,
				Rules: []kubernetes.PolicyRule{
					{APIGroups: []string{""}, Resources: []string{"pods", "endpoints"}, Verbs: []string{"get", "list", "watch"}},
				},
			},
		},
		{
			Type: TypeClusterRoleBinding, Name: cfg.Name + "-service-discovery",
			Desired: kubernetes.ClusterRoleBinding{
				Name:   cfg.Name + "-service-discovery",
				Labels: labels,
				RoleRef: kubernetes.RoleRef{
					APIGroup: "rbac.authorization.k8s.io",
					Kind:     "ClusterRole",
					Name:     cfg.Name + "-service-discovery",
				},
				Subjects: []kubernetes.Subject{
					{Kind: "ServiceAccount", Name: cfg.Name, Namespace: cfg.Namespace},
				},
			},
		},
		{
			Type: TypeConfigMap, Namespace: cfg.Namespace, Name: cfg.Name + "-config",
			Desired: kubernetes.ConfigMap{
				Name: cfg.Name + "-config", Namespace: cfg.Namespace, Labels: labels,
				Data: map[string]string{
					"vault.hcl": `ui = true

disable_mlock = true
api_addr = "http://vault.borq-system.svc:8200"

listener "tcp" {
  tls_disable = 1
  address = "[::]:8200"
  cluster_address = "[::]:8201"
}

storage "file" {
  path = "/vault/data"
}
`,
				},
			},
		},
		{
			Type: TypeService, Namespace: cfg.Namespace, Name: cfg.Name,
			Desired: kubernetes.Service{
				Name: cfg.Name, Namespace: cfg.Namespace, Labels: labels, Selector: selector,
				Type: kubernetes.ClusterIPService,
				Ports: []kubernetes.ServicePort{
					{Name: "api", Port: 8200, TargetPort: 8200},
					{Name: "cluster", Port: 8201, TargetPort: 8201},
				},
			},
		},
		{
			Type: TypeService, Namespace: cfg.Namespace, Name: cfg.Name + "-internal",
			Desired: kubernetes.Service{
				Name: cfg.Name + "-internal", Namespace: cfg.Namespace, Labels: labels, Selector: selector,
				Type:                     kubernetes.ClusterIPService,
				Headless:                 true,
				PublishNotReadyAddresses: true,
				Ports: []kubernetes.ServicePort{
					{Name: "api", Port: 8200, TargetPort: 8200},
					{Name: "cluster", Port: 8201, TargetPort: 8201},
				},
			},
		},
		{
			Type: TypePersistentVolumeClaim, Namespace: cfg.Namespace, Name: cfg.Name + "-data",
			Desired: kubernetes.PersistentVolumeClaim{
				Name: cfg.Name + "-data", Namespace: cfg.Namespace, Labels: labels,
				AccessModes:      []kubernetes.PersistentVolumeAccessMode{kubernetes.ReadWriteOnceVolume},
				RequestedStorage: cfg.StorageSize,
			},
		},
		{
			Type: TypeDeployment, Namespace: cfg.Namespace, Name: cfg.Name,
			Desired: kubernetes.Deployment{
				Name: cfg.Name, Namespace: cfg.Namespace, Labels: labels, Selector: selector,
				Replicas: cfg.Replicas,
				Pods: kubernetes.PodTemplate{
					Labels:             podLabels,
					ServiceAccountName: cfg.Name,
					Volumes: []kubernetes.Volume{
						{Name: "data", PersistentVolumeClaimName: cfg.Name + "-data"},
						{Name: "config", ConfigMapName: cfg.Name + "-config"},
					},
					Containers: []kubernetes.Container{
						{
							Name:    cfg.Name,
							Image:   cfg.Image,
							Command: []string{"vault"},
							Args:    []string{"server", "-config", "/vault/config/vault.hcl"},
							Env: []kubernetes.EnvVar{
								{Name: "SKIP_CHOWN", Value: "true"},
								{Name: "SKIP_SETCAP", Value: "true"},
							},
							Ports: []kubernetes.ContainerPort{
								{Name: "api", Port: 8200},
								{Name: "cluster", Port: 8201},
							},
							VolumeMounts: []kubernetes.VolumeMount{
								{Name: "data", MountPath: "/vault/data"},
								{Name: "config", MountPath: "/vault/config"},
							},
							ReadinessProbe: kubernetes.HttpProbe{
								Path:                "/v1/sys/health?standbyok=true&sealedcode=200",
								Port:                8200,
								InitialDelaySeconds: 5,
								PeriodSeconds:       5,
							},
						},
					},
				},
			},
		},
	}
}

var vaultComponentLabels = map[string]string{
	"app.kubernetes.io/managed-by": "borq",
	"app.kubernetes.io/component":  "vault",
}

const vaultUnsealSecretName = "vault-unseal"

// ApplyResource dispatches on resource type.
func (vh VaultHandler) ApplyResource(ctx context.Context, conn kubernetes.Connection, res Resource) error {
	if res.Type == vaultInitType {
		return vh.reconcileVaultState(ctx, conn, res)
	}
	return vh.CoreApplier.ApplyResource(ctx, conn, res)
}

// DeleteResource dispatches on resource type.
func (vh VaultHandler) DeleteResource(ctx context.Context, conn kubernetes.Connection, res Resource) error {
	if res.Type == vaultInitType {
		return nil
	}
	return vh.CoreApplier.DeleteResource(ctx, conn, res)
}

// reconcileVaultState initializes vault if needed and unseals it.
func (vh VaultHandler) reconcileVaultState(ctx context.Context, conn kubernetes.Connection, res Resource) error {
	vh.Log.Debug("vault_reconcile_start", logger.WithFields(
		logger.String("resource", res.Type+"/"+res.Name),
		logger.String("namespace", res.Namespace),
	))

	if err := vh.Client.WaitForReady(ctx, time.Second, 30*time.Second); err != nil {
		return fmt.Errorf("vault: wait for ready: %w", err)
	}
	vh.Log.Debug("vault_ready")

	status, err := vh.Client.Status(ctx)
	if err != nil {
		return fmt.Errorf("vault: check status: %w", err)
	}
	vh.Log.Debug("vault_status", logger.WithFields(
		logger.Bool("initialized", status.Initialized),
		logger.Bool("sealed", status.Sealed),
	))

	if !status.Initialized {
		vh.Log.Info("vault_init_start")
		result, err := vh.Client.Init(ctx)
		if err != nil {
			return fmt.Errorf("vault: init: %w", err)
		}
		vh.Log.Info("vault_init_complete")

		keysJSON, _ := json.Marshal(result.UnsealKeysB64)

		secret := kubernetes.Secret{
			Name:      vaultUnsealSecretName,
			Namespace: res.Namespace,
			Labels:    vaultComponentLabels,
			Data: map[string]string{
				"root_token":      result.RootToken,
				"unseal_key":      result.UnsealKeysB64[0],
				"unseal_keys_b64": string(keysJSON),
			},
		}
		if err := conn.Secrets().Apply(ctx, secret); err != nil {
			return fmt.Errorf("vault: store unseal keys: %w", err)
		}
		vh.Log.Debug("vault_unseal_secret_stored")

		if err := vh.Client.Unseal(ctx, result.UnsealKeysB64[0]); err != nil {
			return fmt.Errorf("vault: unseal after init: %w", err)
		}
		vh.Log.Info("vault_unsealed_after_init")

		return nil
	}

	if status.Sealed {
		vh.Log.Info("vault_unseal_from_stored_key")
		secret, err := conn.Secrets().Get(ctx, res.Namespace, vaultUnsealSecretName)
		if err != nil {
			return fmt.Errorf("vault: sealed and no stored unseal key found: %w", err)
		}

		unsealKey, ok := secret.Data["unseal_key"]
		if !ok {
			return fmt.Errorf("vault: sealed and secret %q has no 'unseal_key' field", vaultUnsealSecretName)
		}

		if err := vh.Client.Unseal(ctx, unsealKey); err != nil {
			return fmt.Errorf("vault: unseal from stored key: %w", err)
		}
		vh.Log.Info("vault_unsealed_from_stored_key")
	}

	vh.Log.Debug("vault_reconcile_complete")
	return nil
}

var vaultApplyOrder = []string{
	TypeServiceAccount,
	TypeClusterRole,
	TypeClusterRoleBinding,
	TypeConfigMap,
	TypeService,
	TypePersistentVolumeClaim,
	TypeDeployment,
	vaultInitType,
}
