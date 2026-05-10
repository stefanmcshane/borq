package reconciler

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	"github.com/stefanmcshane/borq/foundations/openbao"
)

const openBaoInitType = "OpenBaoInit"

// OpenBaoConfig describes an OpenBao server to reconcile.
type OpenBaoConfig struct {
	Name        string `json:"name"`
	Namespace   string `json:"namespace"`
	Image       string `json:"image"`
	Replicas    int32  `json:"replicas"`
	StorageSize string `json:"storageSize"`
}

// OpenBaoState captures the live operational state of an OpenBao instance.
type OpenBaoState struct {
	Initialized bool
	Sealed      bool
}

// OpenBaoHandler implements ComponentHandler for the openbao component.
type OpenBaoHandler struct {
	CoreApplier
	Client *openbao.Client
	Log    logger.Logger
}

func (OpenBaoHandler) Name() string { return "openbao" }

func (OpenBaoHandler) DefaultConfig() any {
	return &OpenBaoConfig{
		Name: "openbao", Namespace: "borq-system",
		Image: "ghcr.io/openbao/openbao:2.4.4", Replicas: 1, StorageSize: "10Gi",
	}
}

func (handler OpenBaoHandler) Plan(config any) Plan {
	cfg := config.(*OpenBaoConfig)
	if cfg.Name == "" {
		cfg.Name = "openbao"
	}
	if cfg.Namespace == "" {
		cfg.Namespace = "borq-system"
	}
	if cfg.Image == "" {
		cfg.Image = "ghcr.io/openbao/openbao:2.4.4"
	}
	if cfg.Replicas <= 0 {
		cfg.Replicas = 1
	}
	if cfg.StorageSize == "" {
		cfg.StorageSize = "10Gi"
	}

	labels := openBaoComponentLabels
	selector := map[string]string{"app.kubernetes.io/name": cfg.Name}
	podLabels := map[string]string{
		"app.kubernetes.io/name":       cfg.Name,
		"app.kubernetes.io/managed-by": "borq",
		"app.kubernetes.io/component":  "openbao",
	}

	plan := Plan{
		ApplyOrder: openBaoApplyOrder,
		Desired:    openBaoResources(*cfg, labels, selector, podLabels),
		Current:    openBaoCurrentFunc(handler.Log, handler.Client, *cfg, labels, selector),
	}

	return plan
}

func openBaoCurrentFunc(log logger.Logger, client *openbao.Client, cfg OpenBaoConfig, componentLabels, selector map[string]string) func(ctx context.Context, conn kubernetes.Connection) ([]Resource, error) {
	return func(ctx context.Context, conn kubernetes.Connection) ([]Resource, error) {
		var resources []Resource

		k8sResources, err := listOpenBaoK8sResources(ctx, conn, cfg.Namespace, componentLabels, selector)
		if err != nil {
			return nil, err
		}
		resources = append(resources, k8sResources...)

		log.Debug("openbao_current_discover_state")
		openBaoState, err := discoverOpenBaoState(ctx, client)
		if err != nil {
			log.Debug("openbao_current_state_unreachable", logger.WithFields(
				logger.String("error", err.Error()),
			))
			resources = append(resources, Resource{
				Type: openBaoInitType, Name: cfg.Name + "-state",
				Actual: OpenBaoState{Initialized: false, Sealed: true},
			})
			return resources, nil
		}
		log.Debug("openbao_current_state", logger.WithFields(
			logger.Bool("initialized", openBaoState.Initialized),
			logger.Bool("sealed", openBaoState.Sealed),
		))
		resources = append(resources, Resource{
			Type: openBaoInitType, Name: cfg.Name + "-state",
			Actual: openBaoState,
		})
		return resources, nil
	}
}

func listOpenBaoK8sResources(ctx context.Context, conn kubernetes.Connection, namespace string, componentLabels, selector map[string]string) ([]Resource, error) {
	labelSelector := "app.kubernetes.io/managed-by=borq,app.kubernetes.io/component=openbao"
	var resources []Resource

	svcs, err := conn.Services().List(ctx, namespace, labelSelector)
	if err != nil {
		return nil, fmt.Errorf("list openbao services: %w", err)
	}
	for _, s := range svcs {
		resources = append(resources, Resource{Type: TypeService, Namespace: namespace, Name: s.Name, Actual: s})
	}

	cms, err := conn.ConfigMaps().List(ctx, namespace, labelSelector)
	if err != nil {
		return nil, fmt.Errorf("list openbao configmaps: %w", err)
	}
	for _, cm := range cms {
		resources = append(resources, Resource{Type: TypeConfigMap, Namespace: namespace, Name: cm.Name, Actual: cm})
	}

	sas, err := conn.ServiceAccounts().List(ctx, namespace, labelSelector)
	if err != nil {
		return nil, fmt.Errorf("list openbao serviceaccounts: %w", err)
	}
	for _, sa := range sas {
		resources = append(resources, Resource{Type: TypeServiceAccount, Namespace: namespace, Name: sa.Name, Actual: sa})
	}

	pvcs, err := conn.PersistentVolumeClaims().List(ctx, namespace, labelSelector)
	if err != nil {
		return nil, fmt.Errorf("list openbao pvcs: %w", err)
	}
	for _, pvc := range pvcs {
		resources = append(resources, Resource{Type: TypePersistentVolumeClaim, Namespace: namespace, Name: pvc.Name, Actual: pvc})
	}

	deploys, err := conn.Deployments().List(ctx, namespace, labelSelector)
	if err != nil {
		return nil, fmt.Errorf("list openbao deployments: %w", err)
	}
	for _, d := range deploys {
		resources = append(resources, Resource{Type: TypeDeployment, Namespace: namespace, Name: d.Name, Actual: d})
	}

	secrets, err := conn.Secrets().List(ctx, namespace, labelSelector)
	if err != nil {
		return nil, fmt.Errorf("list openbao secrets: %w", err)
	}
	for _, sec := range secrets {
		resources = append(resources, Resource{Type: TypeSecret, Namespace: namespace, Name: sec.Name, Actual: sec})
	}

	crs, err := conn.ClusterRoles().List(ctx, labelSelector)
	if err != nil {
		return nil, fmt.Errorf("list openbao clusterroles: %w", err)
	}
	for _, cr := range crs {
		resources = append(resources, Resource{Type: TypeClusterRole, Name: cr.Name, Actual: cr})
	}

	crbs, err := conn.ClusterRoleBindings().List(ctx, labelSelector)
	if err != nil {
		return nil, fmt.Errorf("list openbao clusterrolebindings: %w", err)
	}
	for _, crb := range crbs {
		resources = append(resources, Resource{Type: TypeClusterRoleBinding, Name: crb.Name, Actual: crb})
	}

	return resources, nil
}

func discoverOpenBaoState(ctx context.Context, client *openbao.Client) (OpenBaoState, error) {
	status, err := client.Status(ctx)
	if err != nil {
		return OpenBaoState{}, err
	}
	return OpenBaoState{Initialized: status.Initialized, Sealed: status.Sealed}, nil
}

func openBaoResources(cfg OpenBaoConfig, labels, selector, podLabels map[string]string) []Resource {
	return append(
		openBaoK8sResources(cfg, labels, selector, podLabels),
		Resource{
			Type:      openBaoInitType,
			Namespace: cfg.Namespace,
			Name:      cfg.Name + "-state",
			Desired:   OpenBaoState{Initialized: true, Sealed: false},
		},
	)
}

func openBaoK8sResources(cfg OpenBaoConfig, labels, selector, podLabels map[string]string) []Resource {
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
					"openbao.hcl": `ui = true

disable_mlock = true
api_addr = "http://openbao.borq-system.svc:8200"

listener "tcp" {
  tls_disable = 1
  address = "[::]:8200"
  cluster_address = "[::]:8201"
}

storage "file" {
  path = "/openbao/data"
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
							Command: []string{"bao"},
							Args:    []string{"server", "-config", "/openbao/config/openbao.hcl"},
							Env: []kubernetes.EnvVar{
								{Name: "SKIP_CHOWN", Value: "true"},
								{Name: "SKIP_SETCAP", Value: "true"},
							},
							Ports: []kubernetes.ContainerPort{
								{Name: "api", Port: 8200},
								{Name: "cluster", Port: 8201},
							},
							VolumeMounts: []kubernetes.VolumeMount{
								{Name: "data", MountPath: "/openbao/data"},
								{Name: "config", MountPath: "/openbao/config"},
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

var openBaoComponentLabels = map[string]string{
	"app.kubernetes.io/managed-by": "borq",
	"app.kubernetes.io/component":  "openbao",
}

const openBaoUnsealSecretName = "openbao-unseal"

// ApplyResource dispatches on resource type.
func (handler OpenBaoHandler) ApplyResource(ctx context.Context, conn kubernetes.Connection, res Resource) error {
	if res.Type == openBaoInitType {
		return handler.reconcileOpenBaoState(ctx, conn, res)
	}
	return handler.CoreApplier.ApplyResource(ctx, conn, res)
}

// DeleteResource dispatches on resource type.
func (handler OpenBaoHandler) DeleteResource(ctx context.Context, conn kubernetes.Connection, res Resource) error {
	if res.Type == openBaoInitType {
		return nil
	}
	return handler.CoreApplier.DeleteResource(ctx, conn, res)
}

// reconcileOpenBaoState initializes OpenBao if needed and unseals it.
func (handler OpenBaoHandler) reconcileOpenBaoState(ctx context.Context, conn kubernetes.Connection, res Resource) error {
	handler.Log.Debug("openbao_reconcile_start", logger.WithFields(
		logger.String("resource", res.Type+"/"+res.Name),
		logger.String("namespace", res.Namespace),
	))

	if err := handler.Client.WaitForReady(ctx, time.Second, 30*time.Second); err != nil {
		return fmt.Errorf("openbao: wait for ready: %w", err)
	}
	handler.Log.Debug("openbao_ready")

	status, err := handler.Client.Status(ctx)
	if err != nil {
		return fmt.Errorf("openbao: check status: %w", err)
	}
	handler.Log.Debug("openbao_status", logger.WithFields(
		logger.Bool("initialized", status.Initialized),
		logger.Bool("sealed", status.Sealed),
	))

	if !status.Initialized {
		handler.Log.Info("openbao_init_start")
		result, err := handler.Client.Init(ctx)
		if err != nil {
			return fmt.Errorf("openbao: init: %w", err)
		}
		handler.Log.Info("openbao_init_complete")

		keysJSON, _ := json.Marshal(result.UnsealKeysB64)

		secret := kubernetes.Secret{
			Name:      openBaoUnsealSecretName,
			Namespace: res.Namespace,
			Labels:    openBaoComponentLabels,
			Data: map[string]string{
				"root_token":      result.RootToken,
				"unseal_key":      result.UnsealKeysB64[0],
				"unseal_keys_b64": string(keysJSON),
			},
		}
		if err := conn.Secrets().Apply(ctx, secret); err != nil {
			return fmt.Errorf("openbao: store unseal keys: %w", err)
		}
		handler.Log.Debug("openbao_unseal_secret_stored")

		if err := handler.Client.Unseal(ctx, result.UnsealKeysB64[0]); err != nil {
			return fmt.Errorf("openbao: unseal after init: %w", err)
		}
		handler.Log.Info("openbao_unsealed_after_init")

		return nil
	}

	if status.Sealed {
		handler.Log.Info("openbao_unseal_from_stored_key")
		secret, err := conn.Secrets().Get(ctx, res.Namespace, openBaoUnsealSecretName)
		if err != nil {
			return fmt.Errorf("openbao: sealed and no stored unseal key found: %w", err)
		}

		unsealKey, ok := secret.Data["unseal_key"]
		if !ok {
			return fmt.Errorf("openbao: sealed and secret %q has no 'unseal_key' field", openBaoUnsealSecretName)
		}

		if err := handler.Client.Unseal(ctx, unsealKey); err != nil {
			return fmt.Errorf("openbao: unseal from stored key: %w", err)
		}
		handler.Log.Info("openbao_unsealed_from_stored_key")
	}

	handler.Log.Debug("openbao_reconcile_complete")
	return nil
}

var openBaoApplyOrder = []string{
	TypeServiceAccount,
	TypeClusterRole,
	TypeClusterRoleBinding,
	TypeConfigMap,
	TypeService,
	TypePersistentVolumeClaim,
	TypeDeployment,
	openBaoInitType,
}
