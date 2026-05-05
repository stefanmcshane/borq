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
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/yaml"
)

const defaultFieldOwner = "borq"

// Option configures a Kubernetes connection.
type Option func(*connectionConfig) error

type connectionConfig struct {
	fieldOwner string
}

// Config describes how to connect to Kubernetes.
type Config struct {
	ContextName string
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
	Labels     map[string]string
	Containers []Container
}

// Container is a borq-local container manifest.
type Container struct {
	Name           string
	Image          string
	Command        []string
	Args           []string
	Env            []EnvVar
	Ports          []ContainerPort
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
	if strings.TrimSpace(config.ContextName) == "" {
		return Connection{}, errors.New("context name must not be empty")
	}

	connectionConfig, err := buildConnectionConfig(options)
	if err != nil {
		return Connection{}, err
	}

	restConfig, err := restConfigForContext(config.ContextName)
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

// Deployments returns a manager for Deployment resources.
func (connection Connection) Deployments() DeploymentManager {
	return DeploymentManager{
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
	manager.logger.Info("applying service", logger.String("namespace", service.Namespace), logger.String("name", service.Name))

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
	manager.logger.Info("deleting service", logger.String("namespace", namespace), logger.String("name", name))

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
	manager.logger.Info("applying deployment", logger.String("namespace", deployment.Namespace), logger.String("name", deployment.Name))

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
	manager.logger.Info("deleting deployment", logger.String("namespace", namespace), logger.String("name", name))

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
	manager.logger.Info("waiting for deployment availability", logger.String("namespace", namespace), logger.String("name", name))

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
	manager.logger.Info("applying statefulset", logger.String("namespace", statefulSet.Namespace), logger.String("name", statefulSet.Name))

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
	manager.logger.Info("deleting statefulset", logger.String("namespace", namespace), logger.String("name", name))

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
	manager.logger.Info("waiting for statefulset readiness", logger.String("namespace", namespace), logger.String("name", name))

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
	manager.logger.Info("applying namespace", logger.String("namespace", namespace.Name))

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
	manager.logger.Info("deleting namespace", logger.String("namespace", name))

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
	manager.logger.Info("applying pod", logger.String("namespace", pod.Namespace), logger.String("name", pod.Name))

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
	manager.logger.Info("deleting pod", logger.String("namespace", namespace), logger.String("name", name))

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

func restConfigForContext(contextName string) (rest.Config, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{CurrentContext: contextName}
	config := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides)

	rawConfig, err := config.RawConfig()
	if err != nil {
		return rest.Config{}, fmt.Errorf("load kubeconfig: %w", err)
	}
	if _, ok := rawConfig.Contexts[contextName]; !ok {
		return rest.Config{}, fmt.Errorf("kubeconfig context %q not found", contextName)
	}

	restConfig, err := config.ClientConfig()
	if err != nil {
		return rest.Config{}, fmt.Errorf("build rest config for context %q: %w", contextName, err)
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
	containers := make([]Container, 0, len(spec.Containers))
	for _, container := range spec.Containers {
		containers = append(containers, containerFromResource(container))
	}

	return PodTemplate{Labels: cloneLabels(labels), Containers: containers}
}

func podSpecToResource(template PodTemplate) corev1.PodSpec {
	containers := make([]corev1.Container, 0, len(template.Containers))
	for _, container := range template.Containers {
		containers = append(containers, containerToResource(container))
	}

	return corev1.PodSpec{Containers: containers}
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

	resource := corev1.Container{
		Name:    container.Name,
		Image:   container.Image,
		Command: cloneStrings(container.Command),
		Args:    cloneStrings(container.Args),
		Env:     resourceEnv,
		Ports:   resourcePorts,
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
		ReadinessProbe: probe,
	}
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
