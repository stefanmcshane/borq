package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/stefanmcshane/borq/foundations/logger"

	appsv1 "k8s.io/api/apps/v1"
	authv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/yaml"
)

const (
	defaultFieldOwner                 = "borq"
	defaultStorageClassAnnotation     = "storageclass.kubernetes.io/is-default-class"
	defaultStorageClassBetaAnnotation = "storageclass.beta.kubernetes.io/is-default-class"
)

// Option configures a Kubernetes connection.
type Option func(*connectionConfig) error

type connectionConfig struct {
	fieldOwner string
}

// Config describes how to connect to Kubernetes.
type Config struct {
	KubeconfigPath string
}

// DiffResult describes how a desired resource differs from the live cluster object.
type DiffResult struct {
	Changed bool
	Desired string
	Live    string
}

// Namespace is a borq-local namespace manifest.
type Namespace struct {
	Name   string
	Labels map[string]string
}

// Service is a borq-local service manifest.
type Service struct {
	Name                     string
	Namespace                string
	Labels                   map[string]string
	Selector                 map[string]string
	Type                     ServiceType
	Headless                 bool
	PublishNotReadyAddresses bool
	Ports                    []ServicePort
}

// ServiceType is a borq-local service type.
type ServiceType string

const (
	// ClusterIPService is a cluster-internal service.
	ClusterIPService ServiceType = "ClusterIP"
	// NodePortService is a nodeport service.
	NodePortService ServiceType = "NodePort"
)

// ServicePort is a borq-local service port.
type ServicePort struct {
	Name       string
	Port       int32
	TargetPort int32
	NodePort   int32
}

// PersistentVolumeClaim is a borq-local persistent volume claim manifest.
type PersistentVolumeClaim struct {
	Name             string
	Namespace        string
	Labels           map[string]string
	AccessModes      []PersistentVolumeAccessMode
	StorageClassName string
	RequestedStorage string
}

// ConfigMap is a borq-local config map manifest.
type ConfigMap struct {
	Name      string
	Namespace string
	Labels    map[string]string
	Data      map[string]string
}

// ServiceAccount is a borq-local service account manifest.
type ServiceAccount struct {
	Name      string
	Namespace string
	Labels    map[string]string
}

// Role is a borq-local role manifest.
type Role struct {
	Name      string
	Namespace string
	Labels    map[string]string
	Rules     []PolicyRule
}

// PolicyRule is a borq-local RBAC policy rule.
type PolicyRule struct {
	APIGroups []string
	Resources []string
	Verbs     []string
}

// SelfSubjectAccessReviewInput describes a namespace-scoped SSAR request.
type SelfSubjectAccessReviewInput struct {
	// Namespace is the namespace to check.
	Namespace string
	// Rule is the namespace-scoped RBAC rule to review.
	Rule PolicyRule
}

// RoleBinding is a borq-local role binding manifest.
type RoleBinding struct {
	Name      string
	Namespace string
	Labels    map[string]string
	RoleRef   RoleRef
	Subjects  []Subject
}

// RoleRef is a borq-local RBAC role reference.
type RoleRef struct {
	APIGroup string
	Kind     string
	Name     string
}

// Subject is a borq-local RBAC subject.
type Subject struct {
	Kind      string
	Name      string
	Namespace string
}

// ClusterRole is a borq-local cluster role manifest.
type ClusterRole struct {
	Name   string
	Labels map[string]string
	Rules  []PolicyRule
}

// ClusterRoleBinding is a borq-local cluster role binding manifest.
type ClusterRoleBinding struct {
	Name     string
	Labels   map[string]string
	RoleRef  RoleRef
	Subjects []Subject
}

// PersistentVolumeAccessMode is a borq-local access mode.
type PersistentVolumeAccessMode string

const (
	// ReadWriteOnceVolume can be mounted read-write by a single node.
	ReadWriteOnceVolume PersistentVolumeAccessMode = "ReadWriteOnce"
)

// Deployment is a borq-local deployment manifest.
type Deployment struct {
	Name      string
	Namespace string
	Labels    map[string]string
	Selector  map[string]string
	Replicas  int32
	Pods      PodTemplate
}

// StatefulSet is a borq-local statefulset manifest.
type StatefulSet struct {
	Name        string
	Namespace   string
	Labels      map[string]string
	Selector    map[string]string
	Replicas    int32
	ServiceName string
	Pods        PodTemplate
}

// Pod is a borq-local pod manifest.
type Pod struct {
	Name      string
	Namespace string
	Labels    map[string]string
	Spec      PodTemplate
}

// PodTemplate is a borq-local pod template.
type PodTemplate struct {
	Labels             map[string]string
	ServiceAccountName string
	Volumes            []Volume
	Containers         []Container
}

// Container is a borq-local container manifest.
type Container struct {
	Name           string
	Image          string
	Command        []string
	Args           []string
	Env            []EnvVar
	Ports          []ContainerPort
	VolumeMounts   []VolumeMount
	ReadinessProbe HttpProbe
}

// EnvVar is a borq-local environment variable.
type EnvVar struct {
	Name  string
	Value string
}

// ContainerPort is a borq-local container port.
type ContainerPort struct {
	Name string
	Port int32
}

// Volume is a borq-local pod volume.
type Volume struct {
	Name                      string
	PersistentVolumeClaimName string
}

// VolumeMount is a borq-local container volume mount.
type VolumeMount struct {
	Name      string
	MountPath string
}

// HttpProbe is a borq-local HTTP probe.
type HttpProbe struct {
	Path                string
	Port                int32
	InitialDelaySeconds int32
	PeriodSeconds       int32
}

// Connection provides pleasant access to Kubernetes resource managers.
type Connection struct {
	clientset  kubernetes.Interface
	fieldOwner string
	logger     logger.Logger
}

// NewConnection creates a Kubernetes connection.
func NewConnection(config Config, log logger.Logger, options ...Option) (Connection, error) {
	connectionConfig, err := buildConnectionConfig(options)
	if err != nil {
		return Connection{}, err
	}

	restConfig, err := restConfigForConfig(config)
	if err != nil {
		return Connection{}, err
	}

	clientset, err := kubernetes.NewForConfig(&restConfig)
	if err != nil {
		return Connection{}, fmt.Errorf("create kubernetes clientset: %w", err)
	}

	return Connection{
		clientset:  clientset,
		fieldOwner: connectionConfig.fieldOwner,
		logger:     log,
	}, nil
}

// Services returns a manager for Service resources.
func (connection Connection) Services() ServiceManager {
	return ServiceManager{
		clientset:  connection.clientset,
		fieldOwner: connection.fieldOwner,
		logger:     connection.logger,
	}
}

// ConfigMaps returns a manager for ConfigMap resources.
func (connection Connection) ConfigMaps() ConfigMapManager {
	return ConfigMapManager{
		clientset:  connection.clientset,
		fieldOwner: connection.fieldOwner,
		logger:     connection.logger,
	}
}

// ServiceAccounts returns a manager for ServiceAccount resources.
func (connection Connection) ServiceAccounts() ServiceAccountManager {
	return ServiceAccountManager{
		clientset:  connection.clientset,
		fieldOwner: connection.fieldOwner,
		logger:     connection.logger,
	}
}

// Roles returns a manager for Role resources.
func (connection Connection) Roles() RoleManager {
	return RoleManager{
		clientset:  connection.clientset,
		fieldOwner: connection.fieldOwner,
		logger:     connection.logger,
	}
}

// RoleBindings returns a manager for RoleBinding resources.
func (connection Connection) RoleBindings() RoleBindingManager {
	return RoleBindingManager{
		clientset:  connection.clientset,
		fieldOwner: connection.fieldOwner,
		logger:     connection.logger,
	}
}

// Deployments returns a manager for Deployment resources.
func (connection Connection) Deployments() DeploymentManager {
	return DeploymentManager{
		clientset:  connection.clientset,
		fieldOwner: connection.fieldOwner,
		logger:     connection.logger,
	}
}

// PersistentVolumeClaims returns a manager for PersistentVolumeClaim resources.
func (connection Connection) PersistentVolumeClaims() PersistentVolumeClaimManager {
	return PersistentVolumeClaimManager{
		clientset:  connection.clientset,
		fieldOwner: connection.fieldOwner,
		logger:     connection.logger,
	}
}

// StorageClasses returns a manager for StorageClass resources.
func (connection Connection) StorageClasses() StorageClassManager {
	return StorageClassManager{
		clientset:  connection.clientset,
		fieldOwner: connection.fieldOwner,
		logger:     connection.logger,
	}
}

// StatefulSets returns a manager for StatefulSet resources.
func (connection Connection) StatefulSets() StatefulSetManager {
	return StatefulSetManager{
		clientset:  connection.clientset,
		fieldOwner: connection.fieldOwner,
		logger:     connection.logger,
	}
}

// Namespaces returns a manager for Namespace resources.
func (connection Connection) Namespaces() NamespaceManager {
	return NamespaceManager{
		clientset:  connection.clientset,
		fieldOwner: connection.fieldOwner,
		logger:     connection.logger,
	}
}

// Pods returns a manager for Pod resources.
func (connection Connection) Pods() PodManager {
	return PodManager{
		clientset:  connection.clientset,
		fieldOwner: connection.fieldOwner,
		logger:     connection.logger,
	}
}

// ClusterRoles returns a manager for ClusterRole resources.
func (connection Connection) ClusterRoles() ClusterRoleManager {
	return ClusterRoleManager{
		clientset:  connection.clientset,
		fieldOwner: connection.fieldOwner,
		logger:     connection.logger,
	}
}

// ClusterRoleBindings returns a manager for ClusterRoleBinding resources.
func (connection Connection) ClusterRoleBindings() ClusterRoleBindingManager {
	return ClusterRoleBindingManager{
		clientset:  connection.clientset,
		fieldOwner: connection.fieldOwner,
		logger:     connection.logger,
	}
}

// CanAdminNamespace reports whether the current credentials can administer namespaced resources.
func (connection Connection) CanAdminNamespace(ctx context.Context, namespace string) error {
	return connection.SelfSubjectAccessReview(ctx, SelfSubjectAccessReviewInput{
		Namespace: namespace,
		Rule: PolicyRule{
			APIGroups: []string{"*"},
			Resources: []string{"*"},
			Verbs:     []string{"*"},
		},
	})
}

// SelfSubjectAccessReview reports whether the provided credentials can perform the requested namespace-scoped rule.
func (connection Connection) SelfSubjectAccessReview(ctx context.Context, input SelfSubjectAccessReviewInput) error {
	namespace := strings.TrimSpace(input.Namespace)
	if namespace == "" {
		return errors.New("namespace must not be empty")
	}
	if len(input.Rule.Verbs) != 1 {
		return errors.New("self subject access review requires exactly one verb")
	}
	if len(input.Rule.Resources) != 1 {
		return errors.New("self subject access review requires exactly one resource")
	}
	if len(input.Rule.APIGroups) > 1 {
		return errors.New("self subject access review requires at most one api group")
	}

	apiGroup := ""
	if len(input.Rule.APIGroups) == 1 {
		apiGroup = input.Rule.APIGroups[0]
	}

	review, err := connection.clientset.AuthorizationV1().SelfSubjectAccessReviews().Create(
		ctx,
		&authv1.SelfSubjectAccessReview{
			Spec: authv1.SelfSubjectAccessReviewSpec{
				ResourceAttributes: &authv1.ResourceAttributes{
					Namespace: namespace,
					Verb:      input.Rule.Verbs[0],
					Group:     apiGroup,
					Resource:  input.Rule.Resources[0],
				},
			},
		},
		metav1.CreateOptions{},
	)
	if err != nil {
		return fmt.Errorf("check namespace access for %q: %w", namespace, err)
	}
	if review.Status.Allowed {
		return nil
	}
	if strings.TrimSpace(review.Status.Reason) != "" {
		return fmt.Errorf("namespace access denied for %q: %s", namespace, review.Status.Reason)
	}

	return fmt.Errorf("namespace access denied for %q", namespace)
}

// ServiceManager manages Service resources.
type ServiceManager struct {
	clientset  kubernetes.Interface
	fieldOwner string
	logger     logger.Logger
}

// Get fetches a Service by namespace and name.
func (manager ServiceManager) Get(ctx context.Context, namespace string, name string) (Service, error) {
	service, err := manager.clientset.CoreV1().Services(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return Service{}, fmt.Errorf("get service %q: %w", name, err)
	}

	return serviceFromResource(*service), nil
}

// List fetches Services in a namespace matching the label selector.
func (manager ServiceManager) List(ctx context.Context, namespace string, selector string) ([]Service, error) {
	serviceList, err := manager.clientset.CoreV1().Services(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("list services in namespace %q: %w", namespace, err)
	}

	services := make([]Service, 0, len(serviceList.Items))
	for _, service := range serviceList.Items {
		services = append(services, serviceFromResource(service))
	}

	return services, nil
}

// Apply creates or updates a Service with server-side apply.
func (manager ServiceManager) Apply(ctx context.Context, service Service) error {
	manager.logger.Debug("applying_service", logger.WithFields(
		logger.String("namespace", service.Namespace),
		logger.String("name", service.Name),
	))

	resource := serviceToResource(service)
	data, err := json.Marshal(resource)
	if err != nil {
		return fmt.Errorf("marshal service %q for apply: %w", service.Name, err)
	}

	force := true
	_, err = manager.clientset.CoreV1().Services(service.Namespace).Patch(
		ctx,
		service.Name,
		types.ApplyPatchType,
		data,
		metav1.PatchOptions{FieldManager: manager.fieldOwner, Force: &force},
	)
	if err != nil {
		return fmt.Errorf("apply service %q: %w", service.Name, err)
	}

	return nil
}

// Delete removes a Service if it exists.
func (manager ServiceManager) Delete(ctx context.Context, namespace string, name string) error {
	manager.logger.Debug("deleting_service", logger.WithFields(
		logger.String("namespace", namespace),
		logger.String("name", name),
	))

	err := manager.clientset.CoreV1().Services(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete service %q: %w", name, err)
	}

	return nil
}

// Diff compares a desired Service against the live cluster object.
func (manager ServiceManager) Diff(ctx context.Context, service Service) (DiffResult, error) {
	live, err := manager.Get(ctx, service.Namespace, service.Name)
	if apierrors.IsNotFound(err) {
		desiredText, marshalErr := marshalYAML(service)
		if marshalErr != nil {
			return DiffResult{}, marshalErr
		}

		return DiffResult{Changed: true, Desired: desiredText}, nil
	}
	if err != nil {
		return DiffResult{}, err
	}

	return diffNormalized(live, service)
}

// DeploymentManager manages Deployment resources.
type DeploymentManager struct {
	clientset  kubernetes.Interface
	fieldOwner string
	logger     logger.Logger
}

// ConfigMapManager manages ConfigMap resources.
type ConfigMapManager struct {
	clientset  kubernetes.Interface
	fieldOwner string
	logger     logger.Logger
}

// ServiceAccountManager manages ServiceAccount resources.
type ServiceAccountManager struct {
	clientset  kubernetes.Interface
	fieldOwner string
	logger     logger.Logger
}

// RoleManager manages Role resources.
type RoleManager struct {
	clientset  kubernetes.Interface
	fieldOwner string
	logger     logger.Logger
}

// RoleBindingManager manages RoleBinding resources.
type RoleBindingManager struct {
	clientset  kubernetes.Interface
	fieldOwner string
	logger     logger.Logger
}

// PersistentVolumeClaimManager manages PersistentVolumeClaim resources.
type PersistentVolumeClaimManager struct {
	clientset  kubernetes.Interface
	fieldOwner string
	logger     logger.Logger
}

// StorageClassManager manages StorageClass resources.
type StorageClassManager struct {
	clientset  kubernetes.Interface
	fieldOwner string
	logger     logger.Logger
}

// ClusterRoleManager manages ClusterRole resources.
type ClusterRoleManager struct {
	clientset  kubernetes.Interface
	fieldOwner string
	logger     logger.Logger
}

// ClusterRoleBindingManager manages ClusterRoleBinding resources.
type ClusterRoleBindingManager struct {
	clientset  kubernetes.Interface
	fieldOwner string
	logger     logger.Logger
}

// Get fetches a ConfigMap by namespace and name.
func (manager ConfigMapManager) Get(ctx context.Context, namespace string, name string) (ConfigMap, error) {
	configMap, err := manager.clientset.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return ConfigMap{}, fmt.Errorf("get config map %q: %w", name, err)
	}

	return configMapFromResource(*configMap), nil
}

// List fetches ConfigMaps in a namespace matching the label selector.
func (manager ConfigMapManager) List(ctx context.Context, namespace string, selector string) ([]ConfigMap, error) {
	configMapList, err := manager.clientset.CoreV1().ConfigMaps(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("list config maps in namespace %q: %w", namespace, err)
	}

	configMaps := make([]ConfigMap, 0, len(configMapList.Items))
	for _, configMap := range configMapList.Items {
		configMaps = append(configMaps, configMapFromResource(configMap))
	}

	return configMaps, nil
}

// Apply creates or updates a ConfigMap with server-side apply.
func (manager ConfigMapManager) Apply(ctx context.Context, configMap ConfigMap) error {
	manager.logger.Debug("applying_config_map", logger.WithFields(
		logger.String("namespace", configMap.Namespace),
		logger.String("name", configMap.Name),
	))

	resource := configMapToResource(configMap)
	data, err := json.Marshal(resource)
	if err != nil {
		return fmt.Errorf("marshal config map %q for apply: %w", configMap.Name, err)
	}

	force := true
	_, err = manager.clientset.CoreV1().ConfigMaps(configMap.Namespace).Patch(
		ctx,
		configMap.Name,
		types.ApplyPatchType,
		data,
		metav1.PatchOptions{FieldManager: manager.fieldOwner, Force: &force},
	)
	if err != nil {
		return fmt.Errorf("apply config map %q: %w", configMap.Name, err)
	}

	return nil
}

// Delete removes a ConfigMap if it exists.
func (manager ConfigMapManager) Delete(ctx context.Context, namespace string, name string) error {
	manager.logger.Debug("deleting_config_map", logger.WithFields(
		logger.String("namespace", namespace),
		logger.String("name", name),
	))

	err := manager.clientset.CoreV1().ConfigMaps(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete config map %q: %w", name, err)
	}

	return nil
}

// Diff compares a desired ConfigMap against the live cluster object.
func (manager ConfigMapManager) Diff(ctx context.Context, configMap ConfigMap) (DiffResult, error) {
	live, err := manager.Get(ctx, configMap.Namespace, configMap.Name)
	if apierrors.IsNotFound(err) {
		desiredText, marshalErr := marshalYAML(configMap)
		if marshalErr != nil {
			return DiffResult{}, marshalErr
		}

		return DiffResult{Changed: true, Desired: desiredText}, nil
	}
	if err != nil {
		return DiffResult{}, err
	}

	return diffNormalized(live, configMap)
}

// Get fetches a ServiceAccount by namespace and name.
func (manager ServiceAccountManager) Get(ctx context.Context, namespace string, name string) (ServiceAccount, error) {
	serviceAccount, err := manager.clientset.CoreV1().ServiceAccounts(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return ServiceAccount{}, fmt.Errorf("get service account %q: %w", name, err)
	}

	return serviceAccountFromResource(*serviceAccount), nil
}

// List fetches ServiceAccounts in a namespace matching the label selector.
func (manager ServiceAccountManager) List(ctx context.Context, namespace string, selector string) ([]ServiceAccount, error) {
	serviceAccountList, err := manager.clientset.CoreV1().ServiceAccounts(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("list service accounts in namespace %q: %w", namespace, err)
	}

	serviceAccounts := make([]ServiceAccount, 0, len(serviceAccountList.Items))
	for _, serviceAccount := range serviceAccountList.Items {
		serviceAccounts = append(serviceAccounts, serviceAccountFromResource(serviceAccount))
	}

	return serviceAccounts, nil
}

// Apply creates or updates a ServiceAccount with server-side apply.
func (manager ServiceAccountManager) Apply(ctx context.Context, serviceAccount ServiceAccount) error {
	manager.logger.Debug("applying_service_account", logger.WithFields(
		logger.String("namespace", serviceAccount.Namespace),
		logger.String("name", serviceAccount.Name),
	))

	resource := serviceAccountToResource(serviceAccount)
	data, err := json.Marshal(resource)
	if err != nil {
		return fmt.Errorf("marshal service account %q for apply: %w", serviceAccount.Name, err)
	}

	force := true
	_, err = manager.clientset.CoreV1().ServiceAccounts(serviceAccount.Namespace).Patch(
		ctx,
		serviceAccount.Name,
		types.ApplyPatchType,
		data,
		metav1.PatchOptions{FieldManager: manager.fieldOwner, Force: &force},
	)
	if err != nil {
		return fmt.Errorf("apply service account %q: %w", serviceAccount.Name, err)
	}

	return nil
}

// Delete removes a ServiceAccount if it exists.
func (manager ServiceAccountManager) Delete(ctx context.Context, namespace string, name string) error {
	manager.logger.Debug("deleting_service_account", logger.WithFields(
		logger.String("namespace", namespace),
		logger.String("name", name),
	))

	err := manager.clientset.CoreV1().ServiceAccounts(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete service account %q: %w", name, err)
	}

	return nil
}

// Diff compares a desired ServiceAccount against the live cluster object.
func (manager ServiceAccountManager) Diff(ctx context.Context, serviceAccount ServiceAccount) (DiffResult, error) {
	live, err := manager.Get(ctx, serviceAccount.Namespace, serviceAccount.Name)
	if apierrors.IsNotFound(err) {
		desiredText, marshalErr := marshalYAML(serviceAccount)
		if marshalErr != nil {
			return DiffResult{}, marshalErr
		}

		return DiffResult{Changed: true, Desired: desiredText}, nil
	}
	if err != nil {
		return DiffResult{}, err
	}

	return diffNormalized(live, serviceAccount)
}

// Get fetches a Role by namespace and name.
func (manager RoleManager) Get(ctx context.Context, namespace string, name string) (Role, error) {
	role, err := manager.clientset.RbacV1().Roles(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return Role{}, fmt.Errorf("get role %q: %w", name, err)
	}

	return roleFromResource(*role), nil
}

// List fetches Roles in a namespace matching the label selector.
func (manager RoleManager) List(ctx context.Context, namespace string, selector string) ([]Role, error) {
	roleList, err := manager.clientset.RbacV1().Roles(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("list roles in namespace %q: %w", namespace, err)
	}

	roles := make([]Role, 0, len(roleList.Items))
	for _, role := range roleList.Items {
		roles = append(roles, roleFromResource(role))
	}

	return roles, nil
}

// Apply creates or updates a Role with server-side apply.
func (manager RoleManager) Apply(ctx context.Context, role Role) error {
	manager.logger.Debug("applying_role", logger.WithFields(
		logger.String("namespace", role.Namespace),
		logger.String("name", role.Name),
	))

	resource := roleToResource(role)
	data, err := json.Marshal(resource)
	if err != nil {
		return fmt.Errorf("marshal role %q for apply: %w", role.Name, err)
	}

	force := true
	_, err = manager.clientset.RbacV1().Roles(role.Namespace).Patch(
		ctx,
		role.Name,
		types.ApplyPatchType,
		data,
		metav1.PatchOptions{FieldManager: manager.fieldOwner, Force: &force},
	)
	if err != nil {
		return fmt.Errorf("apply role %q: %w", role.Name, err)
	}

	return nil
}

// Delete removes a Role if it exists.
func (manager RoleManager) Delete(ctx context.Context, namespace string, name string) error {
	manager.logger.Debug("deleting_role", logger.WithFields(
		logger.String("namespace", namespace),
		logger.String("name", name),
	))

	err := manager.clientset.RbacV1().Roles(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete role %q: %w", name, err)
	}

	return nil
}

// Diff compares a desired Role against the live cluster object.
func (manager RoleManager) Diff(ctx context.Context, role Role) (DiffResult, error) {
	live, err := manager.Get(ctx, role.Namespace, role.Name)
	if apierrors.IsNotFound(err) {
		desiredText, marshalErr := marshalYAML(role)
		if marshalErr != nil {
			return DiffResult{}, marshalErr
		}

		return DiffResult{Changed: true, Desired: desiredText}, nil
	}
	if err != nil {
		return DiffResult{}, err
	}

	return diffNormalized(live, role)
}

// Get fetches a RoleBinding by namespace and name.
func (manager RoleBindingManager) Get(ctx context.Context, namespace string, name string) (RoleBinding, error) {
	roleBinding, err := manager.clientset.RbacV1().RoleBindings(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return RoleBinding{}, fmt.Errorf("get role binding %q: %w", name, err)
	}

	return roleBindingFromResource(*roleBinding), nil
}

// List fetches RoleBindings in a namespace matching the label selector.
func (manager RoleBindingManager) List(ctx context.Context, namespace string, selector string) ([]RoleBinding, error) {
	roleBindingList, err := manager.clientset.RbacV1().RoleBindings(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("list role bindings in namespace %q: %w", namespace, err)
	}

	roleBindings := make([]RoleBinding, 0, len(roleBindingList.Items))
	for _, roleBinding := range roleBindingList.Items {
		roleBindings = append(roleBindings, roleBindingFromResource(roleBinding))
	}

	return roleBindings, nil
}

// Apply creates or updates a RoleBinding with server-side apply.
func (manager RoleBindingManager) Apply(ctx context.Context, roleBinding RoleBinding) error {
	manager.logger.Debug("applying_role_binding", logger.WithFields(
		logger.String("namespace", roleBinding.Namespace),
		logger.String("name", roleBinding.Name),
	))

	resource := roleBindingToResource(roleBinding)
	data, err := json.Marshal(resource)
	if err != nil {
		return fmt.Errorf("marshal role binding %q for apply: %w", roleBinding.Name, err)
	}

	force := true
	_, err = manager.clientset.RbacV1().RoleBindings(roleBinding.Namespace).Patch(
		ctx,
		roleBinding.Name,
		types.ApplyPatchType,
		data,
		metav1.PatchOptions{FieldManager: manager.fieldOwner, Force: &force},
	)
	if err != nil {
		return fmt.Errorf("apply role binding %q: %w", roleBinding.Name, err)
	}

	return nil
}

// Delete removes a RoleBinding if it exists.
func (manager RoleBindingManager) Delete(ctx context.Context, namespace string, name string) error {
	manager.logger.Debug("deleting_role_binding", logger.WithFields(
		logger.String("namespace", namespace),
		logger.String("name", name),
	))

	err := manager.clientset.RbacV1().RoleBindings(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete role binding %q: %w", name, err)
	}

	return nil
}

// Diff compares a desired RoleBinding against the live cluster object.
func (manager RoleBindingManager) Diff(ctx context.Context, roleBinding RoleBinding) (DiffResult, error) {
	live, err := manager.Get(ctx, roleBinding.Namespace, roleBinding.Name)
	if apierrors.IsNotFound(err) {
		desiredText, marshalErr := marshalYAML(roleBinding)
		if marshalErr != nil {
			return DiffResult{}, marshalErr
		}

		return DiffResult{Changed: true, Desired: desiredText}, nil
	}
	if err != nil {
		return DiffResult{}, err
	}

	return diffNormalized(live, roleBinding)
}

// Get fetches a PersistentVolumeClaim by namespace and name.
func (manager PersistentVolumeClaimManager) Get(ctx context.Context, namespace string, name string) (PersistentVolumeClaim, error) {
	persistentVolumeClaim, err := manager.clientset.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return PersistentVolumeClaim{}, fmt.Errorf("get persistent volume claim %q: %w", name, err)
	}

	return persistentVolumeClaimFromResource(*persistentVolumeClaim), nil
}

// List fetches PersistentVolumeClaims in a namespace matching the label selector.
func (manager PersistentVolumeClaimManager) List(ctx context.Context, namespace string, selector string) ([]PersistentVolumeClaim, error) {
	persistentVolumeClaimList, err := manager.clientset.CoreV1().PersistentVolumeClaims(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("list persistent volume claims in namespace %q: %w", namespace, err)
	}

	persistentVolumeClaims := make([]PersistentVolumeClaim, 0, len(persistentVolumeClaimList.Items))
	for _, persistentVolumeClaim := range persistentVolumeClaimList.Items {
		persistentVolumeClaims = append(persistentVolumeClaims, persistentVolumeClaimFromResource(persistentVolumeClaim))
	}

	return persistentVolumeClaims, nil
}

// Apply creates or updates a PersistentVolumeClaim with server-side apply.
func (manager PersistentVolumeClaimManager) Apply(ctx context.Context, persistentVolumeClaim PersistentVolumeClaim) error {
	manager.logger.Debug("applying_persistent_volume_claim", logger.WithFields(
		logger.String("namespace", persistentVolumeClaim.Namespace),
		logger.String("name", persistentVolumeClaim.Name),
	))

	resource := persistentVolumeClaimToResource(persistentVolumeClaim)
	data, err := json.Marshal(resource)
	if err != nil {
		return fmt.Errorf("marshal persistent volume claim %q for apply: %w", persistentVolumeClaim.Name, err)
	}

	force := true
	_, err = manager.clientset.CoreV1().PersistentVolumeClaims(persistentVolumeClaim.Namespace).Patch(
		ctx,
		persistentVolumeClaim.Name,
		types.ApplyPatchType,
		data,
		metav1.PatchOptions{FieldManager: manager.fieldOwner, Force: &force},
	)
	if err != nil {
		return fmt.Errorf("apply persistent volume claim %q: %w", persistentVolumeClaim.Name, err)
	}

	return nil
}

// Delete removes a PersistentVolumeClaim if it exists.
func (manager PersistentVolumeClaimManager) Delete(ctx context.Context, namespace string, name string) error {
	manager.logger.Debug("deleting_persistent_volume_claim", logger.WithFields(
		logger.String("namespace", namespace),
		logger.String("name", name),
	))

	err := manager.clientset.CoreV1().PersistentVolumeClaims(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete persistent volume claim %q: %w", name, err)
	}

	return nil
}

// Diff compares a desired PersistentVolumeClaim against the live cluster object.
func (manager PersistentVolumeClaimManager) Diff(ctx context.Context, persistentVolumeClaim PersistentVolumeClaim) (DiffResult, error) {
	live, err := manager.Get(ctx, persistentVolumeClaim.Namespace, persistentVolumeClaim.Name)
	if apierrors.IsNotFound(err) {
		desiredText, marshalErr := marshalYAML(persistentVolumeClaim)
		if marshalErr != nil {
			return DiffResult{}, marshalErr
		}

		return DiffResult{Changed: true, Desired: desiredText}, nil
	}
	if err != nil {
		return DiffResult{}, err
	}

	return diffNormalized(live, persistentVolumeClaim)
}

// WaitForBound waits until the PersistentVolumeClaim is bound.
func (manager PersistentVolumeClaimManager) WaitForBound(ctx context.Context, namespace string, name string) error {
	manager.logger.Debug("waiting_for_persistent_volume_claim_binding", logger.WithFields(
		logger.String("namespace", namespace),
		logger.String("name", name),
	))

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		persistentVolumeClaim, err := manager.clientset.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				select {
				case <-ctx.Done():
					return fmt.Errorf("persistent volume claim %q was never created before timeout: %w", name, ctx.Err())
				case <-ticker.C:
					continue
				}
			}
			return fmt.Errorf("get persistent volume claim %q: %w", name, err)
		}

		if persistentVolumeClaim.Status.Phase == corev1.ClaimBound {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("persistent volume claim %q did not bind before timeout: %w", name, ctx.Err())
		case <-ticker.C:
		}
	}
}

// EnsureRetainDefault ensures the named StorageClass is the cluster default and uses Retain reclaim policy.
func (manager StorageClassManager) EnsureRetainDefault(ctx context.Context, name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("storage class name must not be empty")
	}

	storageClassList, err := manager.clientset.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list storage classes: %w", err)
	}

	sourceStorageClass, err := sourceStorageClassForRetain(storageClassList.Items, name)
	if err != nil {
		return err
	}

	manager.logger.Debug("ensuring_retain_storage_class_is_default", logger.WithFields(
		logger.String("name", name),
		logger.String("source_name", sourceStorageClass.Name),
	))

	if err := manager.applyStorageClass(ctx, retainStorageClass(sourceStorageClass, name)); err != nil {
		return err
	}

	for _, storageClass := range storageClassList.Items {
		if storageClass.Name == name || !isDefaultStorageClass(storageClass) {
			continue
		}

		if err := manager.setDefaultStorageClass(ctx, storageClass.Name, false); err != nil {
			return err
		}
	}

	if sourceStorageClass.Name == name && !isDefaultStorageClass(sourceStorageClass) {
		if err := manager.setDefaultStorageClass(ctx, name, true); err != nil {
			return err
		}
	}

	return nil
}

// Get fetches a Deployment by namespace and name.
func (manager DeploymentManager) Get(ctx context.Context, namespace string, name string) (Deployment, error) {
	deployment, err := manager.clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return Deployment{}, fmt.Errorf("get deployment %q: %w", name, err)
	}

	return deploymentFromResource(*deployment), nil
}

// List fetches Deployments in a namespace matching the label selector.
func (manager DeploymentManager) List(ctx context.Context, namespace string, selector string) ([]Deployment, error) {
	deploymentList, err := manager.clientset.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("list deployments in namespace %q: %w", namespace, err)
	}

	deployments := make([]Deployment, 0, len(deploymentList.Items))
	for _, deployment := range deploymentList.Items {
		deployments = append(deployments, deploymentFromResource(deployment))
	}

	return deployments, nil
}

// Apply creates or updates a Deployment with server-side apply.
func (manager DeploymentManager) Apply(ctx context.Context, deployment Deployment) error {
	manager.logger.Debug("applying_deployment", logger.WithFields(
		logger.String("namespace", deployment.Namespace),
		logger.String("name", deployment.Name),
	))

	resource := deploymentToResource(deployment)
	data, err := json.Marshal(resource)
	if err != nil {
		return fmt.Errorf("marshal deployment %q for apply: %w", deployment.Name, err)
	}

	force := true
	_, err = manager.clientset.AppsV1().Deployments(deployment.Namespace).Patch(
		ctx,
		deployment.Name,
		types.ApplyPatchType,
		data,
		metav1.PatchOptions{FieldManager: manager.fieldOwner, Force: &force},
	)
	if err != nil {
		return fmt.Errorf("apply deployment %q: %w", deployment.Name, err)
	}

	return nil
}

// Delete removes a Deployment if it exists.
func (manager DeploymentManager) Delete(ctx context.Context, namespace string, name string) error {
	manager.logger.Debug("deleting_deployment", logger.WithFields(
		logger.String("namespace", namespace),
		logger.String("name", name),
	))

	err := manager.clientset.AppsV1().Deployments(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete deployment %q: %w", name, err)
	}

	return nil
}

// Diff compares a desired Deployment against the live cluster object.
func (manager DeploymentManager) Diff(ctx context.Context, deployment Deployment) (DiffResult, error) {
	live, err := manager.Get(ctx, deployment.Namespace, deployment.Name)
	if apierrors.IsNotFound(err) {
		desiredText, marshalErr := marshalYAML(deployment)
		if marshalErr != nil {
			return DiffResult{}, marshalErr
		}

		return DiffResult{Changed: true, Desired: desiredText}, nil
	}
	if err != nil {
		return DiffResult{}, err
	}

	return diffNormalized(live, deployment)
}

// WaitForAvailable waits until the Deployment has at least one available replica.
func (manager DeploymentManager) WaitForAvailable(ctx context.Context, namespace string, name string) error {
	manager.logger.Debug("waiting_for_deployment_availability", logger.WithFields(
		logger.String("namespace", namespace),
		logger.String("name", name),
	))

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		deployment, err := manager.clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				select {
				case <-ctx.Done():
					return fmt.Errorf("deployment %q was never created before timeout: %w", name, ctx.Err())
				case <-ticker.C:
					continue
				}
			}
			return fmt.Errorf("get deployment %q: %w", name, err)
		}

		if deploymentAvailable(*deployment) {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("deployment %q did not become available before timeout: %w", name, ctx.Err())
		case <-ticker.C:
		}
	}
}

// StatefulSetManager manages StatefulSet resources.
type StatefulSetManager struct {
	clientset  kubernetes.Interface
	fieldOwner string
	logger     logger.Logger
}

// Get fetches a StatefulSet by namespace and name.
func (manager StatefulSetManager) Get(ctx context.Context, namespace string, name string) (StatefulSet, error) {
	statefulSet, err := manager.clientset.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return StatefulSet{}, fmt.Errorf("get statefulset %q: %w", name, err)
	}

	return statefulSetFromResource(*statefulSet), nil
}

// List fetches StatefulSets in a namespace matching the label selector.
func (manager StatefulSetManager) List(ctx context.Context, namespace string, selector string) ([]StatefulSet, error) {
	statefulSetList, err := manager.clientset.AppsV1().StatefulSets(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("list statefulsets in namespace %q: %w", namespace, err)
	}

	statefulSets := make([]StatefulSet, 0, len(statefulSetList.Items))
	for _, statefulSet := range statefulSetList.Items {
		statefulSets = append(statefulSets, statefulSetFromResource(statefulSet))
	}

	return statefulSets, nil
}

// Apply creates or updates a StatefulSet with server-side apply.
func (manager StatefulSetManager) Apply(ctx context.Context, statefulSet StatefulSet) error {
	manager.logger.Debug("applying_statefulset", logger.WithFields(
		logger.String("namespace", statefulSet.Namespace),
		logger.String("name", statefulSet.Name),
	))

	resource := statefulSetToResource(statefulSet)
	data, err := json.Marshal(resource)
	if err != nil {
		return fmt.Errorf("marshal statefulset %q for apply: %w", statefulSet.Name, err)
	}

	force := true
	_, err = manager.clientset.AppsV1().StatefulSets(statefulSet.Namespace).Patch(
		ctx,
		statefulSet.Name,
		types.ApplyPatchType,
		data,
		metav1.PatchOptions{FieldManager: manager.fieldOwner, Force: &force},
	)
	if err != nil {
		return fmt.Errorf("apply statefulset %q: %w", statefulSet.Name, err)
	}

	return nil
}

// Delete removes a StatefulSet if it exists.
func (manager StatefulSetManager) Delete(ctx context.Context, namespace string, name string) error {
	manager.logger.Debug("deleting_statefulset", logger.WithFields(
		logger.String("namespace", namespace),
		logger.String("name", name),
	))

	err := manager.clientset.AppsV1().StatefulSets(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete statefulset %q: %w", name, err)
	}

	return nil
}

// Diff compares a desired StatefulSet against the live cluster object.
func (manager StatefulSetManager) Diff(ctx context.Context, statefulSet StatefulSet) (DiffResult, error) {
	live, err := manager.Get(ctx, statefulSet.Namespace, statefulSet.Name)
	if apierrors.IsNotFound(err) {
		desiredText, marshalErr := marshalYAML(statefulSet)
		if marshalErr != nil {
			return DiffResult{}, marshalErr
		}

		return DiffResult{Changed: true, Desired: desiredText}, nil
	}
	if err != nil {
		return DiffResult{}, err
	}

	return diffNormalized(live, statefulSet)
}

// WaitForReady waits until the StatefulSet has at least one ready replica.
func (manager StatefulSetManager) WaitForReady(ctx context.Context, namespace string, name string) error {
	manager.logger.Debug("waiting_for_statefulset_readiness", logger.WithFields(
		logger.String("namespace", namespace),
		logger.String("name", name),
	))

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		statefulSet, err := manager.clientset.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				select {
				case <-ctx.Done():
					return fmt.Errorf("statefulset %q was never created before timeout: %w", name, ctx.Err())
				case <-ticker.C:
					continue
				}
			}
			return fmt.Errorf("get statefulset %q: %w", name, err)
		}

		if statefulSet.Status.ReadyReplicas >= 1 {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("statefulset %q did not become ready before timeout: %w", name, ctx.Err())
		case <-ticker.C:
		}
	}
}

// NamespaceManager manages Namespace resources.
type NamespaceManager struct {
	clientset  kubernetes.Interface
	fieldOwner string
	logger     logger.Logger
}

// Get fetches a Namespace by name.
func (manager NamespaceManager) Get(ctx context.Context, name string) (Namespace, error) {
	namespace, err := manager.clientset.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return Namespace{}, fmt.Errorf("get namespace %q: %w", name, err)
	}

	return namespaceFromResource(*namespace), nil
}

// List fetches Namespaces matching the label selector.
func (manager NamespaceManager) List(ctx context.Context, selector string) ([]Namespace, error) {
	namespaceList, err := manager.clientset.CoreV1().Namespaces().List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("list namespaces: %w", err)
	}

	namespaces := make([]Namespace, 0, len(namespaceList.Items))
	for _, namespace := range namespaceList.Items {
		namespaces = append(namespaces, namespaceFromResource(namespace))
	}

	return namespaces, nil
}

// Apply creates or updates a Namespace with server-side apply.
func (manager NamespaceManager) Apply(ctx context.Context, namespace Namespace) error {
	manager.logger.Debug("applying_namespace", logger.WithFields(
		logger.String("namespace", namespace.Name),
	))

	resource := namespaceToResource(namespace)
	data, err := json.Marshal(resource)
	if err != nil {
		return fmt.Errorf("marshal namespace %q for apply: %w", namespace.Name, err)
	}

	force := true
	_, err = manager.clientset.CoreV1().Namespaces().Patch(
		ctx,
		namespace.Name,
		types.ApplyPatchType,
		data,
		metav1.PatchOptions{FieldManager: manager.fieldOwner, Force: &force},
	)
	if err != nil {
		return fmt.Errorf("apply namespace %q: %w", namespace.Name, err)
	}

	return nil
}

// Delete removes a Namespace if it exists.
func (manager NamespaceManager) Delete(ctx context.Context, name string) error {
	manager.logger.Debug("deleting_namespace", logger.WithFields(
		logger.String("namespace", name),
	))

	err := manager.clientset.CoreV1().Namespaces().Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete namespace %q: %w", name, err)
	}

	return nil
}

// Diff compares a desired Namespace against the live cluster object.
func (manager NamespaceManager) Diff(ctx context.Context, namespace Namespace) (DiffResult, error) {
	live, err := manager.Get(ctx, namespace.Name)
	if apierrors.IsNotFound(err) {
		desiredText, marshalErr := marshalYAML(namespace)
		if marshalErr != nil {
			return DiffResult{}, marshalErr
		}

		return DiffResult{Changed: true, Desired: desiredText}, nil
	}
	if err != nil {
		return DiffResult{}, err
	}

	return diffNormalized(live, namespace)
}

// PodManager manages Pod resources.
type PodManager struct {
	clientset  kubernetes.Interface
	fieldOwner string
	logger     logger.Logger
}

// Get fetches a Pod by namespace and name.
func (manager PodManager) Get(ctx context.Context, namespace string, name string) (Pod, error) {
	pod, err := manager.clientset.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return Pod{}, fmt.Errorf("get pod %q: %w", name, err)
	}

	return podFromResource(*pod), nil
}

// List fetches Pods in a namespace matching the label selector.
func (manager PodManager) List(ctx context.Context, namespace string, selector string) ([]Pod, error) {
	podList, err := manager.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("list pods in namespace %q: %w", namespace, err)
	}

	pods := make([]Pod, 0, len(podList.Items))
	for _, pod := range podList.Items {
		pods = append(pods, podFromResource(pod))
	}

	return pods, nil
}

// Apply creates or updates a Pod with server-side apply.
func (manager PodManager) Apply(ctx context.Context, pod Pod) error {
	manager.logger.Debug("applying_pod", logger.WithFields(
		logger.String("namespace", pod.Namespace),
		logger.String("name", pod.Name),
	))

	resource := podToResource(pod)
	data, err := json.Marshal(resource)
	if err != nil {
		return fmt.Errorf("marshal pod %q for apply: %w", pod.Name, err)
	}

	force := true
	_, err = manager.clientset.CoreV1().Pods(pod.Namespace).Patch(
		ctx,
		pod.Name,
		types.ApplyPatchType,
		data,
		metav1.PatchOptions{FieldManager: manager.fieldOwner, Force: &force},
	)
	if err != nil {
		return fmt.Errorf("apply pod %q: %w", pod.Name, err)
	}

	return nil
}

// Delete removes a Pod if it exists.
func (manager PodManager) Delete(ctx context.Context, namespace string, name string) error {
	manager.logger.Debug("deleting_pod", logger.WithFields(
		logger.String("namespace", namespace),
		logger.String("name", name),
	))

	err := manager.clientset.CoreV1().Pods(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete pod %q: %w", name, err)
	}

	return nil
}

// Diff compares a desired Pod against the live cluster object.
func (manager PodManager) Diff(ctx context.Context, pod Pod) (DiffResult, error) {
	live, err := manager.Get(ctx, pod.Namespace, pod.Name)
	if apierrors.IsNotFound(err) {
		desiredText, marshalErr := marshalYAML(pod)
		if marshalErr != nil {
			return DiffResult{}, marshalErr
		}

		return DiffResult{Changed: true, Desired: desiredText}, nil
	}
	if err != nil {
		return DiffResult{}, err
	}

	return diffNormalized(live, pod)
}

func buildConnectionConfig(options []Option) (connectionConfig, error) {
	config := connectionConfig{fieldOwner: defaultFieldOwner}
	for _, option := range options {
		if err := option(&config); err != nil {
			return connectionConfig{}, err
		}
	}

	return config, nil
}

func restConfigForConfig(config Config) (rest.Config, error) {
	kubeconfigPath := strings.TrimSpace(config.KubeconfigPath)
	if kubeconfigPath == "" {
		restConfig, err := rest.InClusterConfig()
		if err != nil {
			return rest.Config{}, fmt.Errorf("load in-cluster kubernetes config: %w", err)
		}
		return *restConfig, nil
	}

	loadingRules := &clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfigPath}
	clientConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, &clientcmd.ConfigOverrides{})
	restConfig, err := clientConfig.ClientConfig()
	if err != nil {
		return rest.Config{}, fmt.Errorf("load kubeconfig %q: %w", kubeconfigPath, err)
	}

	return *restConfig, nil
}

func diffNormalized(live any, desired any) (DiffResult, error) {
	liveText, err := marshalYAML(live)
	if err != nil {
		return DiffResult{}, err
	}
	desiredText, err := marshalYAML(desired)
	if err != nil {
		return DiffResult{}, err
	}

	return DiffResult{Changed: liveText != desiredText, Desired: desiredText, Live: liveText}, nil
}

func marshalYAML(value any) (string, error) {
	data, err := yaml.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("marshal diff yaml: %w", err)
	}

	return string(data), nil
}

func serviceTypeToResource(serviceType ServiceType) corev1.ServiceType {
	if serviceType == NodePortService {
		return corev1.ServiceTypeNodePort
	}

	return corev1.ServiceTypeClusterIP
}

func serviceTypeFromResource(serviceType corev1.ServiceType) ServiceType {
	if serviceType == corev1.ServiceTypeNodePort {
		return NodePortService
	}

	return ClusterIPService
}

func namespaceToResource(namespace Namespace) corev1.Namespace {
	return corev1.Namespace{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
		ObjectMeta: metav1.ObjectMeta{Name: namespace.Name, Labels: cloneLabels(namespace.Labels)},
	}
}

func namespaceFromResource(namespace corev1.Namespace) Namespace {
	return Namespace{Name: namespace.Name, Labels: cloneLabels(namespace.Labels)}
}

func configMapToResource(configMap ConfigMap) corev1.ConfigMap {
	return corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      configMap.Name,
			Namespace: configMap.Namespace,
			Labels:    cloneLabels(configMap.Labels),
		},
		Data: cloneLabels(configMap.Data),
	}
}

func configMapFromResource(configMap corev1.ConfigMap) ConfigMap {
	return ConfigMap{
		Name:      configMap.Name,
		Namespace: configMap.Namespace,
		Labels:    cloneLabels(configMap.Labels),
		Data:      cloneLabels(configMap.Data),
	}
}

func serviceAccountToResource(serviceAccount ServiceAccount) corev1.ServiceAccount {
	return corev1.ServiceAccount{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceAccount.Name,
			Namespace: serviceAccount.Namespace,
			Labels:    cloneLabels(serviceAccount.Labels),
		},
	}
}

func serviceAccountFromResource(serviceAccount corev1.ServiceAccount) ServiceAccount {
	return ServiceAccount{
		Name:      serviceAccount.Name,
		Namespace: serviceAccount.Namespace,
		Labels:    cloneLabels(serviceAccount.Labels),
	}
}

func roleToResource(role Role) rbacv1.Role {
	resourceRules := make([]rbacv1.PolicyRule, 0, len(role.Rules))
	for _, rule := range role.Rules {
		resourceRules = append(resourceRules, rbacv1.PolicyRule{
			APIGroups: cloneStrings(rule.APIGroups),
			Resources: cloneStrings(rule.Resources),
			Verbs:     cloneStrings(rule.Verbs),
		})
	}

	return rbacv1.Role{
		TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      role.Name,
			Namespace: role.Namespace,
			Labels:    cloneLabels(role.Labels),
		},
		Rules: resourceRules,
	}
}

func roleFromResource(role rbacv1.Role) Role {
	resourceRules := make([]PolicyRule, 0, len(role.Rules))
	for _, rule := range role.Rules {
		resourceRules = append(resourceRules, PolicyRule{
			APIGroups: cloneStrings(rule.APIGroups),
			Resources: cloneStrings(rule.Resources),
			Verbs:     cloneStrings(rule.Verbs),
		})
	}

	return Role{
		Name:      role.Name,
		Namespace: role.Namespace,
		Labels:    cloneLabels(role.Labels),
		Rules:     resourceRules,
	}
}

func roleBindingToResource(roleBinding RoleBinding) rbacv1.RoleBinding {
	resourceSubjects := make([]rbacv1.Subject, 0, len(roleBinding.Subjects))
	for _, subject := range roleBinding.Subjects {
		resourceSubjects = append(resourceSubjects, rbacv1.Subject{
			Kind:      subject.Kind,
			Name:      subject.Name,
			Namespace: subject.Namespace,
		})
	}

	return rbacv1.RoleBinding{
		TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      roleBinding.Name,
			Namespace: roleBinding.Namespace,
			Labels:    cloneLabels(roleBinding.Labels),
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: roleBinding.RoleRef.APIGroup,
			Kind:     roleBinding.RoleRef.Kind,
			Name:     roleBinding.RoleRef.Name,
		},
		Subjects: resourceSubjects,
	}
}

func roleBindingFromResource(roleBinding rbacv1.RoleBinding) RoleBinding {
	resourceSubjects := make([]Subject, 0, len(roleBinding.Subjects))
	for _, subject := range roleBinding.Subjects {
		resourceSubjects = append(resourceSubjects, Subject{
			Kind:      subject.Kind,
			Name:      subject.Name,
			Namespace: subject.Namespace,
		})
	}

	return RoleBinding{
		Name:      roleBinding.Name,
		Namespace: roleBinding.Namespace,
		Labels:    cloneLabels(roleBinding.Labels),
		RoleRef: RoleRef{
			APIGroup: roleBinding.RoleRef.APIGroup,
			Kind:     roleBinding.RoleRef.Kind,
			Name:     roleBinding.RoleRef.Name,
		},
		Subjects: resourceSubjects,
	}
}

func serviceToResource(service Service) corev1.Service {
	servicePorts := make([]corev1.ServicePort, 0, len(service.Ports))
	for _, port := range service.Ports {
		servicePorts = append(servicePorts, corev1.ServicePort{
			Name:       port.Name,
			Port:       port.Port,
			TargetPort: int32ToIntOrString(port.TargetPort),
			NodePort:   port.NodePort,
		})
	}

	serviceSpec := corev1.ServiceSpec{
		Type:                     serviceTypeToResource(service.Type),
		Selector:                 cloneLabels(service.Selector),
		Ports:                    servicePorts,
		PublishNotReadyAddresses: service.PublishNotReadyAddresses,
	}
	if service.Headless {
		serviceSpec.ClusterIP = corev1.ClusterIPNone
	}

	return corev1.Service{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{Name: service.Name, Namespace: service.Namespace, Labels: cloneLabels(service.Labels)},
		Spec:       serviceSpec,
	}
}

func serviceFromResource(service corev1.Service) Service {
	servicePorts := make([]ServicePort, 0, len(service.Spec.Ports))
	for _, port := range service.Spec.Ports {
		servicePorts = append(servicePorts, ServicePort{
			Name:       port.Name,
			Port:       port.Port,
			TargetPort: port.TargetPort.IntVal,
			NodePort:   port.NodePort,
		})
	}

	return Service{
		Name:                     service.Name,
		Namespace:                service.Namespace,
		Labels:                   cloneLabels(service.Labels),
		Selector:                 cloneLabels(service.Spec.Selector),
		Type:                     serviceTypeFromResource(service.Spec.Type),
		Headless:                 service.Spec.ClusterIP == corev1.ClusterIPNone,
		PublishNotReadyAddresses: service.Spec.PublishNotReadyAddresses,
		Ports:                    servicePorts,
	}
}

func persistentVolumeClaimToResource(persistentVolumeClaim PersistentVolumeClaim) corev1.PersistentVolumeClaim {
	storageRequirements := corev1.VolumeResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceStorage: resource.MustParse(persistentVolumeClaim.RequestedStorage),
		},
	}

	resourceClaim := corev1.PersistentVolumeClaim{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
		ObjectMeta: metav1.ObjectMeta{Name: persistentVolumeClaim.Name, Namespace: persistentVolumeClaim.Namespace, Labels: cloneLabels(persistentVolumeClaim.Labels)},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: accessModesToResource(persistentVolumeClaim.AccessModes),
			Resources:   storageRequirements,
		},
	}
	if persistentVolumeClaim.StorageClassName != "" {
		resourceClaim.Spec.StorageClassName = &persistentVolumeClaim.StorageClassName
	}

	return resourceClaim
}

func persistentVolumeClaimFromResource(persistentVolumeClaim corev1.PersistentVolumeClaim) PersistentVolumeClaim {
	requestedStorage := ""
	if storage, ok := persistentVolumeClaim.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
		requestedStorage = storage.String()
	}

	storageClassName := ""
	if persistentVolumeClaim.Spec.StorageClassName != nil {
		storageClassName = *persistentVolumeClaim.Spec.StorageClassName
	}

	return PersistentVolumeClaim{
		Name:             persistentVolumeClaim.Name,
		Namespace:        persistentVolumeClaim.Namespace,
		Labels:           cloneLabels(persistentVolumeClaim.Labels),
		AccessModes:      accessModesFromResource(persistentVolumeClaim.Spec.AccessModes),
		StorageClassName: storageClassName,
		RequestedStorage: requestedStorage,
	}
}

func deploymentToResource(deployment Deployment) appsv1.Deployment {
	replicas := deployment.Replicas
	return appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: deployment.Name, Namespace: deployment.Namespace, Labels: cloneLabels(deployment.Labels)},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: cloneLabels(deployment.Selector)},
			Template: podTemplateToResource(deployment.Pods),
		},
	}
}

func deploymentFromResource(deployment appsv1.Deployment) Deployment {
	replicas := int32(1)
	if deployment.Spec.Replicas != nil {
		replicas = *deployment.Spec.Replicas
	}

	return Deployment{
		Name:      deployment.Name,
		Namespace: deployment.Namespace,
		Labels:    cloneLabels(deployment.Labels),
		Selector:  cloneLabels(deployment.Spec.Selector.MatchLabels),
		Replicas:  replicas,
		Pods:      podTemplateFromResource(deployment.Spec.Template),
	}
}

func statefulSetToResource(statefulSet StatefulSet) appsv1.StatefulSet {
	replicas := statefulSet.Replicas
	return appsv1.StatefulSet{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "StatefulSet"},
		ObjectMeta: metav1.ObjectMeta{Name: statefulSet.Name, Namespace: statefulSet.Namespace, Labels: cloneLabels(statefulSet.Labels)},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: statefulSet.ServiceName,
			Replicas:    &replicas,
			Selector:    &metav1.LabelSelector{MatchLabels: cloneLabels(statefulSet.Selector)},
			Template:    podTemplateToResource(statefulSet.Pods),
		},
	}
}

func statefulSetFromResource(statefulSet appsv1.StatefulSet) StatefulSet {
	replicas := int32(1)
	if statefulSet.Spec.Replicas != nil {
		replicas = *statefulSet.Spec.Replicas
	}

	return StatefulSet{
		Name:        statefulSet.Name,
		Namespace:   statefulSet.Namespace,
		Labels:      cloneLabels(statefulSet.Labels),
		Selector:    cloneLabels(statefulSet.Spec.Selector.MatchLabels),
		Replicas:    replicas,
		ServiceName: statefulSet.Spec.ServiceName,
		Pods:        podTemplateFromResource(statefulSet.Spec.Template),
	}
}

func podToResource(pod Pod) corev1.Pod {
	return corev1.Pod{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace, Labels: cloneLabels(pod.Labels)},
		Spec:       podSpecToResource(pod.Spec),
	}
}

func podFromResource(pod corev1.Pod) Pod {
	return Pod{
		Name:      pod.Name,
		Namespace: pod.Namespace,
		Labels:    cloneLabels(pod.Labels),
		Spec:      podTemplateFromPodSpec(pod.Spec, pod.Labels),
	}
}

func podTemplateToResource(template PodTemplate) corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: cloneLabels(template.Labels)},
		Spec:       podSpecToResource(template),
	}
}

func podTemplateFromResource(template corev1.PodTemplateSpec) PodTemplate {
	return podTemplateFromPodSpec(template.Spec, template.Labels)
}

func podTemplateFromPodSpec(spec corev1.PodSpec, labels map[string]string) PodTemplate {
	volumes := make([]Volume, 0, len(spec.Volumes))
	for _, volume := range spec.Volumes {
		volumes = append(volumes, volumeFromResource(volume))
	}

	containers := make([]Container, 0, len(spec.Containers))
	for _, container := range spec.Containers {
		containers = append(containers, containerFromResource(container))
	}

	return PodTemplate{
		Labels:             cloneLabels(labels),
		ServiceAccountName: spec.ServiceAccountName,
		Volumes:            volumes,
		Containers:         containers,
	}
}

func podSpecToResource(template PodTemplate) corev1.PodSpec {
	volumes := make([]corev1.Volume, 0, len(template.Volumes))
	for _, volume := range template.Volumes {
		volumes = append(volumes, volumeToResource(volume))
	}

	containers := make([]corev1.Container, 0, len(template.Containers))
	for _, container := range template.Containers {
		containers = append(containers, containerToResource(container))
	}

	return corev1.PodSpec{
		ServiceAccountName: template.ServiceAccountName,
		Volumes:            volumes,
		Containers:         containers,
	}
}

func containerToResource(container Container) corev1.Container {
	resourcePorts := make([]corev1.ContainerPort, 0, len(container.Ports))
	for _, port := range container.Ports {
		resourcePorts = append(resourcePorts, corev1.ContainerPort{Name: port.Name, ContainerPort: port.Port})
	}

	resourceEnv := make([]corev1.EnvVar, 0, len(container.Env))
	for _, envVar := range container.Env {
		resourceEnv = append(resourceEnv, corev1.EnvVar{Name: envVar.Name, Value: envVar.Value})
	}

	resourceVolumeMounts := make([]corev1.VolumeMount, 0, len(container.VolumeMounts))
	for _, volumeMount := range container.VolumeMounts {
		resourceVolumeMounts = append(resourceVolumeMounts, corev1.VolumeMount{Name: volumeMount.Name, MountPath: volumeMount.MountPath})
	}

	resource := corev1.Container{
		Name:         container.Name,
		Image:        container.Image,
		Command:      cloneStrings(container.Command),
		Args:         cloneStrings(container.Args),
		Env:          resourceEnv,
		Ports:        resourcePorts,
		VolumeMounts: resourceVolumeMounts,
	}
	if container.ReadinessProbe.Path != "" {
		resource.ReadinessProbe = &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: container.ReadinessProbe.Path,
					Port: int32ToIntOrString(container.ReadinessProbe.Port),
				},
			},
			InitialDelaySeconds: container.ReadinessProbe.InitialDelaySeconds,
			PeriodSeconds:       container.ReadinessProbe.PeriodSeconds,
		}
	}

	return resource
}

func containerFromResource(container corev1.Container) Container {
	ports := make([]ContainerPort, 0, len(container.Ports))
	for _, port := range container.Ports {
		ports = append(ports, ContainerPort{Name: port.Name, Port: port.ContainerPort})
	}

	envVars := make([]EnvVar, 0, len(container.Env))
	for _, envVar := range container.Env {
		envVars = append(envVars, EnvVar{Name: envVar.Name, Value: envVar.Value})
	}

	volumeMounts := make([]VolumeMount, 0, len(container.VolumeMounts))
	for _, volumeMount := range container.VolumeMounts {
		volumeMounts = append(volumeMounts, VolumeMount{Name: volumeMount.Name, MountPath: volumeMount.MountPath})
	}

	probe := HttpProbe{}
	if container.ReadinessProbe != nil && container.ReadinessProbe.HTTPGet != nil {
		probe = HttpProbe{
			Path:                container.ReadinessProbe.HTTPGet.Path,
			Port:                container.ReadinessProbe.HTTPGet.Port.IntVal,
			InitialDelaySeconds: container.ReadinessProbe.InitialDelaySeconds,
			PeriodSeconds:       container.ReadinessProbe.PeriodSeconds,
		}
	}

	return Container{
		Name:           container.Name,
		Image:          container.Image,
		Command:        cloneStrings(container.Command),
		Args:           cloneStrings(container.Args),
		Env:            envVars,
		Ports:          ports,
		VolumeMounts:   volumeMounts,
		ReadinessProbe: probe,
	}
}

func volumeToResource(volume Volume) corev1.Volume {
	return corev1.Volume{
		Name: volume.Name,
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: volume.PersistentVolumeClaimName,
			},
		},
	}
}

func volumeFromResource(volume corev1.Volume) Volume {
	persistentVolumeClaimName := ""
	if volume.PersistentVolumeClaim != nil {
		persistentVolumeClaimName = volume.PersistentVolumeClaim.ClaimName
	}

	return Volume{
		Name:                      volume.Name,
		PersistentVolumeClaimName: persistentVolumeClaimName,
	}
}

func accessModesToResource(accessModes []PersistentVolumeAccessMode) []corev1.PersistentVolumeAccessMode {
	resourceAccessModes := make([]corev1.PersistentVolumeAccessMode, 0, len(accessModes))
	for _, accessMode := range accessModes {
		resourceAccessModes = append(resourceAccessModes, corev1.PersistentVolumeAccessMode(accessMode))
	}

	return resourceAccessModes
}

func accessModesFromResource(accessModes []corev1.PersistentVolumeAccessMode) []PersistentVolumeAccessMode {
	resourceAccessModes := make([]PersistentVolumeAccessMode, 0, len(accessModes))
	for _, accessMode := range accessModes {
		resourceAccessModes = append(resourceAccessModes, PersistentVolumeAccessMode(accessMode))
	}

	return resourceAccessModes
}

func (manager StorageClassManager) applyStorageClass(ctx context.Context, storageClass storagev1.StorageClass) error {
	data, err := json.Marshal(storageClass)
	if err != nil {
		return fmt.Errorf("marshal storage class %q for apply: %w", storageClass.Name, err)
	}

	force := true
	_, err = manager.clientset.StorageV1().StorageClasses().Patch(
		ctx,
		storageClass.Name,
		types.ApplyPatchType,
		data,
		metav1.PatchOptions{FieldManager: manager.fieldOwner, Force: &force},
	)
	if apierrors.IsNotFound(err) {
		_, err = manager.clientset.StorageV1().StorageClasses().Create(ctx, &storageClass, metav1.CreateOptions{})
	}
	if err != nil {
		return fmt.Errorf("apply storage class %q: %w", storageClass.Name, err)
	}

	return nil
}

func (manager StorageClassManager) setDefaultStorageClass(ctx context.Context, name string, isDefault bool) error {
	manager.logger.Debug("setting_storage_class_default_state", logger.WithFields(
		logger.String("name", name),
		logger.Bool("is_default", isDefault),
	))

	liveStorageClass, err := manager.clientset.StorageV1().StorageClasses().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get storage class %q: %w", name, err)
	}

	liveStorageClass.Annotations = mergeStorageClassAnnotations(liveStorageClass.Annotations, storageClassDefaultAnnotations(isDefault))
	_, err = manager.clientset.StorageV1().StorageClasses().Update(ctx, liveStorageClass, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("update storage class %q: %w", name, err)
	}

	return nil
}

func sourceStorageClassForRetain(storageClasses []storagev1.StorageClass, retainName string) (storagev1.StorageClass, error) {
	for _, storageClass := range storageClasses {
		if storageClass.Name == retainName {
			return storageClass, nil
		}
	}

	for _, storageClass := range storageClasses {
		if isDefaultStorageClass(storageClass) {
			return storageClass, nil
		}
	}

	return storagev1.StorageClass{}, errors.New("no storage class is available to build retain")
}

func retainStorageClass(sourceStorageClass storagev1.StorageClass, name string) storagev1.StorageClass {
	retainReclaimPolicy := corev1.PersistentVolumeReclaimRetain
	retainStorageClass := storagev1.StorageClass{
		TypeMeta: metav1.TypeMeta{APIVersion: "storage.k8s.io/v1", Kind: "StorageClass"},
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Annotations: storageClassDefaultAnnotations(true),
		},
		Provisioner:   sourceStorageClass.Provisioner,
		Parameters:    cloneLabels(sourceStorageClass.Parameters),
		ReclaimPolicy: &retainReclaimPolicy,
		MountOptions:  cloneStrings(sourceStorageClass.MountOptions),
	}

	if sourceStorageClass.AllowVolumeExpansion != nil {
		allowVolumeExpansion := *sourceStorageClass.AllowVolumeExpansion
		retainStorageClass.AllowVolumeExpansion = &allowVolumeExpansion
	}
	if sourceStorageClass.VolumeBindingMode != nil {
		volumeBindingMode := *sourceStorageClass.VolumeBindingMode
		retainStorageClass.VolumeBindingMode = &volumeBindingMode
	}
	if len(sourceStorageClass.AllowedTopologies) > 0 {
		retainStorageClass.AllowedTopologies = append([]corev1.TopologySelectorTerm(nil), sourceStorageClass.AllowedTopologies...)
	}

	return retainStorageClass
}

func isDefaultStorageClass(storageClass storagev1.StorageClass) bool {
	return storageClass.Annotations[defaultStorageClassAnnotation] == "true" || storageClass.Annotations[defaultStorageClassBetaAnnotation] == "true"
}

func storageClassDefaultAnnotations(isDefault bool) map[string]string {
	defaultValue := "false"
	if isDefault {
		defaultValue = "true"
	}

	return map[string]string{
		defaultStorageClassAnnotation:     defaultValue,
		defaultStorageClassBetaAnnotation: defaultValue,
	}
}

func mergeStorageClassAnnotations(current map[string]string, desired map[string]string) map[string]string {
	merged := cloneLabels(current)
	if merged == nil {
		merged = map[string]string{}
	}

	for key, value := range desired {
		merged[key] = value
	}

	return merged
}

func int32ToIntOrString(value int32) intstr.IntOrString {
	return intstr.FromInt32(value)
}

func cloneLabels(labels map[string]string) map[string]string {
	if len(labels) == 0 {
		return nil
	}

	clonedLabels := make(map[string]string, len(labels))
	for key, value := range labels {
		clonedLabels[key] = value
	}

	return clonedLabels
}

func cloneStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}

	clonedValues := make([]string, len(values))
	copy(clonedValues, values)
	return clonedValues
}

func deploymentAvailable(deployment appsv1.Deployment) bool {
	if deployment.Status.AvailableReplicas < 1 {
		return false
	}

	for _, condition := range deployment.Status.Conditions {
		if condition.Type == appsv1.DeploymentAvailable && condition.Status == corev1.ConditionTrue {
			return true
		}
	}

	return false
}

// Get fetches a ClusterRole by name.
func (manager ClusterRoleManager) Get(ctx context.Context, name string) (ClusterRole, error) {
	clusterRole, err := manager.clientset.RbacV1().ClusterRoles().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return ClusterRole{}, fmt.Errorf("get cluster role %q: %w", name, err)
	}

	return clusterRoleFromResource(*clusterRole), nil
}

// List fetches ClusterRoles matching the label selector.
func (manager ClusterRoleManager) List(ctx context.Context, selector string) ([]ClusterRole, error) {
	clusterRoleList, err := manager.clientset.RbacV1().ClusterRoles().List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("list cluster roles: %w", err)
	}

	clusterRoles := make([]ClusterRole, 0, len(clusterRoleList.Items))
	for _, clusterRole := range clusterRoleList.Items {
		clusterRoles = append(clusterRoles, clusterRoleFromResource(clusterRole))
	}

	return clusterRoles, nil
}

// Apply creates or updates a ClusterRole with server-side apply.
func (manager ClusterRoleManager) Apply(ctx context.Context, clusterRole ClusterRole) error {
	manager.logger.Debug("applying_cluster_role", logger.WithFields(
		logger.String("name", clusterRole.Name),
	))

	resource := clusterRoleToResource(clusterRole)
	data, err := json.Marshal(resource)
	if err != nil {
		return fmt.Errorf("marshal cluster role %q for apply: %w", clusterRole.Name, err)
	}

	force := true
	_, err = manager.clientset.RbacV1().ClusterRoles().Patch(
		ctx,
		clusterRole.Name,
		types.ApplyPatchType,
		data,
		metav1.PatchOptions{FieldManager: manager.fieldOwner, Force: &force},
	)
	if err != nil {
		return fmt.Errorf("apply cluster role %q: %w", clusterRole.Name, err)
	}

	return nil
}

// Delete removes a ClusterRole if it exists.
func (manager ClusterRoleManager) Delete(ctx context.Context, name string) error {
	manager.logger.Debug("deleting_cluster_role", logger.WithFields(
		logger.String("name", name),
	))

	err := manager.clientset.RbacV1().ClusterRoles().Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete cluster role %q: %w", name, err)
	}

	return nil
}

// Diff compares a desired ClusterRole against the live cluster object.
func (manager ClusterRoleManager) Diff(ctx context.Context, clusterRole ClusterRole) (DiffResult, error) {
	live, err := manager.Get(ctx, clusterRole.Name)
	if apierrors.IsNotFound(err) {
		desiredText, marshalErr := marshalYAML(clusterRole)
		if marshalErr != nil {
			return DiffResult{}, marshalErr
		}

		return DiffResult{Changed: true, Desired: desiredText}, nil
	}
	if err != nil {
		return DiffResult{}, err
	}

	return diffNormalized(live, clusterRole)
}

// Get fetches a ClusterRoleBinding by name.
func (manager ClusterRoleBindingManager) Get(ctx context.Context, name string) (ClusterRoleBinding, error) {
	clusterRoleBinding, err := manager.clientset.RbacV1().ClusterRoleBindings().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return ClusterRoleBinding{}, fmt.Errorf("get cluster role binding %q: %w", name, err)
	}

	return clusterRoleBindingFromResource(*clusterRoleBinding), nil
}

// List fetches ClusterRoleBindings matching the label selector.
func (manager ClusterRoleBindingManager) List(ctx context.Context, selector string) ([]ClusterRoleBinding, error) {
	clusterRoleBindingList, err := manager.clientset.RbacV1().ClusterRoleBindings().List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("list cluster role bindings: %w", err)
	}

	clusterRoleBindings := make([]ClusterRoleBinding, 0, len(clusterRoleBindingList.Items))
	for _, clusterRoleBinding := range clusterRoleBindingList.Items {
		clusterRoleBindings = append(clusterRoleBindings, clusterRoleBindingFromResource(clusterRoleBinding))
	}

	return clusterRoleBindings, nil
}

// Apply creates or updates a ClusterRoleBinding with server-side apply.
func (manager ClusterRoleBindingManager) Apply(ctx context.Context, clusterRoleBinding ClusterRoleBinding) error {
	manager.logger.Debug("applying_cluster_role_binding", logger.WithFields(
		logger.String("name", clusterRoleBinding.Name),
	))

	resource := clusterRoleBindingToResource(clusterRoleBinding)
	data, err := json.Marshal(resource)
	if err != nil {
		return fmt.Errorf("marshal cluster role binding %q for apply: %w", clusterRoleBinding.Name, err)
	}

	force := true
	_, err = manager.clientset.RbacV1().ClusterRoleBindings().Patch(
		ctx,
		clusterRoleBinding.Name,
		types.ApplyPatchType,
		data,
		metav1.PatchOptions{FieldManager: manager.fieldOwner, Force: &force},
	)
	if err != nil {
		return fmt.Errorf("apply cluster role binding %q: %w", clusterRoleBinding.Name, err)
	}

	return nil
}

// Delete removes a ClusterRoleBinding if it exists.
func (manager ClusterRoleBindingManager) Delete(ctx context.Context, name string) error {
	manager.logger.Debug("deleting_cluster_role_binding", logger.WithFields(
		logger.String("name", name),
	))

	err := manager.clientset.RbacV1().ClusterRoleBindings().Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete cluster role binding %q: %w", name, err)
	}

	return nil
}

// Diff compares a desired ClusterRoleBinding against the live cluster object.
func (manager ClusterRoleBindingManager) Diff(ctx context.Context, clusterRoleBinding ClusterRoleBinding) (DiffResult, error) {
	live, err := manager.Get(ctx, clusterRoleBinding.Name)
	if apierrors.IsNotFound(err) {
		desiredText, marshalErr := marshalYAML(clusterRoleBinding)
		if marshalErr != nil {
			return DiffResult{}, marshalErr
		}

		return DiffResult{Changed: true, Desired: desiredText}, nil
	}
	if err != nil {
		return DiffResult{}, err
	}

	return diffNormalized(live, clusterRoleBinding)
}

func clusterRoleToResource(clusterRole ClusterRole) rbacv1.ClusterRole {
	resourceRules := make([]rbacv1.PolicyRule, 0, len(clusterRole.Rules))
	for _, rule := range clusterRole.Rules {
		resourceRules = append(resourceRules, rbacv1.PolicyRule{
			APIGroups: cloneStrings(rule.APIGroups),
			Resources: cloneStrings(rule.Resources),
			Verbs:     cloneStrings(rule.Verbs),
		})
	}

	return rbacv1.ClusterRole{
		TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole"},
		ObjectMeta: metav1.ObjectMeta{
			Name:   clusterRole.Name,
			Labels: cloneLabels(clusterRole.Labels),
		},
		Rules: resourceRules,
	}
}

func clusterRoleFromResource(clusterRole rbacv1.ClusterRole) ClusterRole {
	resourceRules := make([]PolicyRule, 0, len(clusterRole.Rules))
	for _, rule := range clusterRole.Rules {
		resourceRules = append(resourceRules, PolicyRule{
			APIGroups: cloneStrings(rule.APIGroups),
			Resources: cloneStrings(rule.Resources),
			Verbs:     cloneStrings(rule.Verbs),
		})
	}

	return ClusterRole{
		Name:   clusterRole.Name,
		Labels: cloneLabels(clusterRole.Labels),
		Rules:  resourceRules,
	}
}

func clusterRoleBindingToResource(clusterRoleBinding ClusterRoleBinding) rbacv1.ClusterRoleBinding {
	resourceSubjects := make([]rbacv1.Subject, 0, len(clusterRoleBinding.Subjects))
	for _, subject := range clusterRoleBinding.Subjects {
		resourceSubjects = append(resourceSubjects, rbacv1.Subject{
			Kind:      subject.Kind,
			Name:      subject.Name,
			Namespace: subject.Namespace,
		})
	}

	return rbacv1.ClusterRoleBinding{
		TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"},
		ObjectMeta: metav1.ObjectMeta{
			Name:   clusterRoleBinding.Name,
			Labels: cloneLabels(clusterRoleBinding.Labels),
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: clusterRoleBinding.RoleRef.APIGroup,
			Kind:     clusterRoleBinding.RoleRef.Kind,
			Name:     clusterRoleBinding.RoleRef.Name,
		},
		Subjects: resourceSubjects,
	}
}

func clusterRoleBindingFromResource(clusterRoleBinding rbacv1.ClusterRoleBinding) ClusterRoleBinding {
	resourceSubjects := make([]Subject, 0, len(clusterRoleBinding.Subjects))
	for _, subject := range clusterRoleBinding.Subjects {
		resourceSubjects = append(resourceSubjects, Subject{
			Kind:      subject.Kind,
			Name:      subject.Name,
			Namespace: subject.Namespace,
		})
	}

	return ClusterRoleBinding{
		Name:   clusterRoleBinding.Name,
		Labels: cloneLabels(clusterRoleBinding.Labels),
		RoleRef: RoleRef{
			APIGroup: clusterRoleBinding.RoleRef.APIGroup,
			Kind:     clusterRoleBinding.RoleRef.Kind,
			Name:     clusterRoleBinding.RoleRef.Name,
		},
		Subjects: resourceSubjects,
	}
}

// WithFieldOwner sets the field manager used for server-side apply.
func WithFieldOwner(fieldOwner string) Option {
	return func(config *connectionConfig) error {
		if strings.TrimSpace(fieldOwner) == "" {
			return errors.New("field owner must not be empty")
		}

		config.fieldOwner = fieldOwner
		return nil
	}
}
