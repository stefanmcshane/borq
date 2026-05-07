package managementreconcile

import (
	"github.com/stefanmcshane/borq/foundations/kubernetes"
)

// VaultConfig describes a vault server to reconcile.
type VaultConfig struct {
	Name        string `json:"name"`
	Namespace   string `json:"namespace"`
	Image       string `json:"image"`
	Replicas    int32  `json:"replicas"`
	StorageSize string `json:"storageSize"`
}

// VaultHandler implements ComponentHandler for the vault component.
type VaultHandler struct{ CoreApplier }

func (VaultHandler) Name() string { return "vault" }

func (VaultHandler) DefaultConfig() any {
	return &VaultConfig{
		Name: "vault", Namespace: "borq-system",
		Image: "hashicorp/vault:1.21.2", Replicas: 1, StorageSize: "10Gi",
	}
}

func (VaultHandler) Plan(config any) ComponentPlan {
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

	return ComponentPlan{
		Selector:   "app.kubernetes.io/managed-by=borq,app.kubernetes.io/component=vault",
		Namespaces: []string{cfg.Namespace},
		NamespaceScopedKinds: []Kind{
			KindServiceAccount,
			KindConfigMap,
			KindService,
			KindPersistentVolumeClaim,
			KindStatefulSet,
		},
		ClusterScopedKinds: []Kind{
			KindClusterRole,
			KindClusterRoleBinding,
		},
		ApplyOrder: vaultApplyOrder,
		Resources:  vaultResources(*cfg, labels, selector, podLabels),
	}
}

func vaultResources(cfg VaultConfig, labels, selector, podLabels map[string]string) []ResourceState {
	return []ResourceState{
		{
			Kind: KindServiceAccount, Namespace: cfg.Namespace, Name: cfg.Name,
			Desired: kubernetes.ServiceAccount{
				Name: cfg.Name, Namespace: cfg.Namespace, Labels: labels,
			},
		},
		{
			Kind: KindClusterRole, Namespace: "", Name: cfg.Name + "-service-discovery",
			Desired: kubernetes.ClusterRole{
				Name:   cfg.Name + "-service-discovery",
				Labels: labels,
				Rules: []kubernetes.PolicyRule{
					{APIGroups: []string{""}, Resources: []string{"pods", "endpoints"}, Verbs: []string{"get", "list", "watch"}},
				},
			},
		},
		{
			Kind: KindClusterRoleBinding, Namespace: "", Name: cfg.Name + "-service-discovery",
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
			Kind: KindConfigMap, Namespace: cfg.Namespace, Name: cfg.Name + "-config",
			Desired: kubernetes.ConfigMap{
				Name: cfg.Name + "-config", Namespace: cfg.Namespace, Labels: labels,
				Data: map[string]string{
					"vault.hcl": `ui = true

listener "tcp" {
  tls_disable = 1
  address = "[::]:8200"
  cluster_address = "[::]:8201"
}

storage "file" {
  path = "/vault/data"
}

service_registration "kubernetes" {}
`,
				},
			},
		},
		{
			Kind: KindService, Namespace: cfg.Namespace, Name: cfg.Name,
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
			Kind: KindService, Namespace: cfg.Namespace, Name: cfg.Name + "-internal",
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
			Kind: KindPersistentVolumeClaim, Namespace: cfg.Namespace, Name: cfg.Name + "-data",
			Desired: kubernetes.PersistentVolumeClaim{
				Name: cfg.Name + "-data", Namespace: cfg.Namespace, Labels: labels,
				AccessModes:      []kubernetes.PersistentVolumeAccessMode{kubernetes.ReadWriteOnceVolume},
				RequestedStorage: cfg.StorageSize,
			},
		},
		{
			Kind: KindStatefulSet, Namespace: cfg.Namespace, Name: cfg.Name,
			Desired: kubernetes.StatefulSet{
				Name: cfg.Name, Namespace: cfg.Namespace, Labels: labels, Selector: selector,
				ServiceName: cfg.Name + "-internal",
				Replicas:    cfg.Replicas,
				Pods: kubernetes.PodTemplate{
					Labels:             podLabels,
					ServiceAccountName: cfg.Name,
					Volumes: []kubernetes.Volume{
						{Name: "data", PersistentVolumeClaimName: cfg.Name + "-data"},
						{Name: "config", PersistentVolumeClaimName: ""},
					},
					Containers: []kubernetes.Container{
						{
							Name:  cfg.Name,
							Image: cfg.Image,
							Args:  []string{"server", "-config", "/vault/config/vault.hcl"},
							Ports: []kubernetes.ContainerPort{
								{Name: "api", Port: 8200},
								{Name: "cluster", Port: 8201},
							},
							VolumeMounts: []kubernetes.VolumeMount{
								{Name: "data", MountPath: "/vault/data"},
								{Name: "config", MountPath: "/vault/config"},
							},
							ReadinessProbe: kubernetes.HttpProbe{
								Path:                "/v1/sys/health?standbyok=true",
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

var vaultApplyOrder = []Kind{
	KindServiceAccount,
	KindClusterRole,
	KindClusterRoleBinding,
	KindConfigMap,
	KindService,
	KindPersistentVolumeClaim,
	KindStatefulSet,
}
