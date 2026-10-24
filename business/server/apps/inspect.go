package apps

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
)

type inspectedResource struct {
	kind     string
	name     string
	app      string
	target   string
	manifest map[string]any
}

// Inspect reconstructs the expanded application model from Borq resources. It
// reads managed fields from the resources themselves, so live manual changes
// remain visible, and rejects shapes that the application model cannot express.
// Controller metadata, Kubernetes defaults, and injected sidecars are ignored.
func Inspect(manifests []map[string]any) (Configuration, error) {
	return InspectWithEnv(manifests, nil)
}

// InspectWithEnv resolves managed Secret references against trusted, versioned
// snapshots. Config references can also be read without any raw secret values.
func InspectWithEnv(manifests []map[string]any, snapshots []EnvSnapshot) (Configuration, error) {
	resources, err := inspectResources(manifests)
	if err != nil {
		return Configuration{}, err
	}
	environments, err := inspectEnvironments(resources, snapshots)
	if err != nil {
		return Configuration{}, err
	}
	applications := make(map[string]App)
	deployments := make(map[string]inspectedResource)
	for _, resource := range resources {
		if resource.kind != "Deployment" {
			continue
		}
		application, err := inspectDeployment(resource, environments[resource.app])
		if err != nil {
			return Configuration{}, fmt.Errorf("Deployment/%s: %w", resource.name, err)
		}
		applications[application.Name] = application
		deployments[application.Name] = resource
	}
	services := make(map[string]bool)
	jobs := make(map[string]bool)
	for _, resource := range resources {
		if resource.kind == "Deployment" {
			continue
		}
		application, exists := applications[resource.app]
		if !exists {
			return Configuration{}, fmt.Errorf("%s/%s has no matching application Deployment", resource.kind, resource.name)
		}
		if resource.kind == "ExternalSecret" {
			continue
		}
		if resource.kind == "Service" {
			if err := inspectService(resource, application); err != nil {
				return Configuration{}, fmt.Errorf("Service/%s: %w", resource.name, err)
			}
			services[application.Name] = true
			continue
		}
		if resource.kind == "Job" {
			if jobs[application.Name] {
				return Configuration{}, fmt.Errorf("app %q has duplicate predeploy Jobs", application.Name)
			}
			task, err := inspectPredeploy(resource, application, environments[resource.app], deployments[resource.app])
			if err != nil {
				return Configuration{}, fmt.Errorf("Job/%s: %w", resource.name, err)
			}
			application.Predeploy = task
			applications[application.Name] = application
			jobs[application.Name] = true
			continue
		}
		ingress, err := inspectIngress(resource, application)
		if err != nil {
			return Configuration{}, fmt.Errorf("Ingress/%s: %w", resource.name, err)
		}
		application.Ingresses = append(application.Ingresses, ingress)
		applications[application.Name] = application
	}
	configuration := Configuration{Apps: make([]App, 0, len(applications))}
	for _, application := range applications {
		if application.Port != 0 && !services[application.Name] {
			return Configuration{}, fmt.Errorf("app %q has a listener but no matching Service", application.Name)
		}
		configuration.Apps = append(configuration.Apps, application)
	}
	return Merge(configuration, Patch{})
}

func inspectResources(manifests []map[string]any) ([]inspectedResource, error) {
	resources := make([]inspectedResource, 0, len(manifests))
	seen := make(map[string]bool, len(manifests))
	namespace, target := "", ""
	for index, manifest := range manifests {
		envelope := struct {
			metav1.TypeMeta   `json:",inline"`
			metav1.ObjectMeta `json:"metadata"`
		}{}
		if err := decodeManifest(manifest, &envelope); err != nil {
			return nil, fmt.Errorf("manifest %d: %w", index, err)
		}
		kind, version := envelope.Kind, envelope.APIVersion
		if !((kind == "Deployment" && version == "apps/v1") ||
			(kind == "Service" && version == "v1") || (kind == "Ingress" && version == "networking.k8s.io/v1") ||
			(kind == "Job" && version == "batch/v1") || (kind == "ExternalSecret" && version == "external-secrets.io/v1")) {
			return nil, fmt.Errorf("manifest %d has unsupported resource %s %s", index, version, kind)
		}
		if envelope.Name == "" || len(validation.IsDNS1123Subdomain(envelope.Name)) > 0 {
			return nil, fmt.Errorf("manifest %d has an invalid resource name", index)
		}
		if len(validation.IsDNS1123Label(envelope.Namespace)) > 0 {
			return nil, fmt.Errorf("%s/%s requires a valid namespace", kind, envelope.Name)
		}
		app, resourceTarget := envelope.Labels["app.kubernetes.io/name"], envelope.Labels["borq.run/target-id"]
		if envelope.Labels["app.kubernetes.io/managed-by"] != "borq" ||
			resourceTarget == "" || len(validation.IsValidLabelValue(resourceTarget)) > 0 ||
			len(validation.IsDNS1035Label(app)) > 0 {
			return nil, fmt.Errorf("%s/%s requires Borq application and target ownership labels", kind, envelope.Name)
		}
		if namespace != "" && (namespace != envelope.Namespace || target != resourceTarget) {
			return nil, fmt.Errorf("manifests must belong to one namespace and Borq target")
		}
		namespace, target = envelope.Namespace, resourceTarget
		identity := kind + "/" + envelope.Name
		if seen[identity] {
			return nil, fmt.Errorf("duplicate resource %s", identity)
		}
		seen[identity] = true
		resources = append(resources, inspectedResource{
			kind: kind, name: envelope.Name, app: app,
			target: target, manifest: manifest,
		})
	}
	return resources, nil
}

func inspectDeployment(resource inspectedResource, environment inspectedEnvironment) (App, error) {
	deployment := appsv1.Deployment{}
	if err := decodeManifest(resource.manifest, &deployment); err != nil {
		return App{}, err
	}
	if resource.name != resource.app {
		return App{}, fmt.Errorf("name must match the application ownership label")
	}
	for key := range deployment.Annotations {
		if strings.HasPrefix(key, "borq.run/disabled-") && key != "borq.run/disabled-readiness-v1" && key != "borq.run/disabled-startup-v1" && key != "borq.run/disabled-predeploy-v1" {
			return App{}, fmt.Errorf("unsupported disabled-check annotation %q", key)
		}
	}
	if deployment.Spec.Selector == nil || len(deployment.Spec.Selector.MatchExpressions) != 0 ||
		!applicationSelector(deployment.Spec.Selector.MatchLabels, resource) {
		return App{}, fmt.Errorf("selector must identify this application and target")
	}
	if deployment.Spec.Template.Labels["app.kubernetes.io/name"] != resource.app ||
		deployment.Spec.Template.Labels["borq.run/target-id"] != resource.target {
		return App{}, fmt.Errorf("pod template must identify this application and target")
	}
	container, found := corev1.Container{}, false
	for _, candidate := range deployment.Spec.Template.Spec.Containers {
		if candidate.Name != resource.app {
			continue
		}
		if found {
			return App{}, fmt.Errorf("duplicate application container")
		}
		container, found = candidate, true
	}
	if !found {
		return App{}, fmt.Errorf("missing application container %q", resource.app)
	}
	listener, err := inspectPort(container.Ports)
	if err != nil {
		return App{}, err
	}
	application := defaultApp(resource.app, &listener)
	application.Image, application.Port = container.Image, listener
	if deployment.Spec.Replicas != nil {
		application.Replicas = int(*deployment.Spec.Replicas)
	}
	application.Command, err = inspectCommand(container.Command, container.Args)
	if err != nil {
		return App{}, fmt.Errorf("command: %w", err)
	}
	if container.LivenessProbe != nil {
		return App{}, fmt.Errorf("liveness probes are not supported by the app model")
	}
	application.Env, err = inspectEnvironmentReference(container.Env, container.EnvFrom, environment, cmp.Or(deployment.Annotations["borq.run/environment-group"], deployment.Annotations["borq.run/env-config"]))
	if err != nil {
		return App{}, err
	}
	application.Checks.Readiness, err = inspectCheck(container.ReadinessProbe, deployment.Annotations["borq.run/disabled-readiness-v1"], application.Checks.Readiness)
	if err != nil {
		return App{}, fmt.Errorf("readiness: %w", err)
	}
	application.Checks.Startup, err = inspectCheck(container.StartupProbe, deployment.Annotations["borq.run/disabled-startup-v1"], application.Checks.Startup)
	if err != nil {
		return App{}, fmt.Errorf("startup: %w", err)
	}
	application.Predeploy, err = inspectDisabledPredeploy(deployment.Annotations["borq.run/disabled-predeploy-v1"])
	if err != nil {
		return App{}, fmt.Errorf("predeploy: %w", err)
	}
	if retained := deployment.Annotations["borq.run/retained-predeploy-v1"]; retained != "" {
		if deployment.Annotations["borq.run/disabled-predeploy-v1"] != "" {
			return App{}, fmt.Errorf("predeploy cannot be both retained and disabled")
		}
		application.Predeploy, err = inspectPredeploySettings(retained)
		if err != nil {
			return App{}, fmt.Errorf("predeploy: %w", err)
		}
	}
	return application, nil
}

func inspectPort(ports []corev1.ContainerPort) (int, error) {
	if len(ports) == 0 {
		return 0, nil
	}
	if len(ports) != 1 {
		return 0, fmt.Errorf("the application must have at most one container port")
	}
	port := ports[0]
	if (port.Name != "" && port.Name != "http") ||
		(port.Protocol != "" && port.Protocol != corev1.ProtocolTCP) || port.HostPort != 0 || port.HostIP != "" {
		return 0, fmt.Errorf("only the application TCP listener is supported")
	}
	if port.ContainerPort < 1 || port.ContainerPort > 65535 {
		return 0, fmt.Errorf("container port must be between 1 and 65535")
	}
	return int(port.ContainerPort), nil
}

// Kubernetes uses pointers to distinguish missing probes and defaulted fields;
// the expanded application always returns a concrete Check.
func inspectCheck(probe *corev1.Probe, annotation string, fallback Check) (Check, error) {
	if probe == nil {
		fallback.Enabled = false
		if annotation == "" {
			return fallback, nil
		}
		if !strings.HasPrefix(strings.TrimSpace(annotation), "{") {
			return Check{}, fmt.Errorf("disabled-check annotation must be a JSON object")
		}
		check := Check{}
		decoder := json.NewDecoder(strings.NewReader(annotation))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&check); err != nil {
			return Check{}, fmt.Errorf("invalid disabled-check annotation: %w", err)
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			return Check{}, fmt.Errorf("disabled-check annotation must contain one JSON object")
		}
		if check.Enabled {
			return Check{}, fmt.Errorf("disabled-check annotation cannot enable a missing probe")
		}
		return check, validateCheck(check)
	}
	if probe.HTTPGet == nil || probe.Exec != nil || probe.TCPSocket != nil || probe.GRPC != nil ||
		probe.InitialDelaySeconds != 0 || (probe.SuccessThreshold != 0 && probe.SuccessThreshold != 1) ||
		probe.TerminationGracePeriodSeconds != nil {
		return Check{}, fmt.Errorf("only HTTP probes with the app model's timing fields are supported")
	}
	request := probe.HTTPGet
	if request.Host != "" || len(request.HTTPHeaders) != 0 || (request.Scheme != "" && request.Scheme != corev1.URISchemeHTTP) {
		return Check{}, fmt.Errorf("probe host, headers and HTTPS are not supported by the app model")
	}
	port, err := applicationPort(request.Port, fallback.Port)
	if err != nil {
		return Check{}, fmt.Errorf("probe: %w", err)
	}
	check := Check{Enabled: true, Path: request.Path, Port: port, PeriodSeconds: 10, TimeoutSeconds: 1, FailureThreshold: 3}
	if check.Path == "" {
		check.Path = "/"
	}
	if probe.PeriodSeconds != 0 {
		check.PeriodSeconds = int(probe.PeriodSeconds)
	}
	if probe.TimeoutSeconds != 0 {
		check.TimeoutSeconds = int(probe.TimeoutSeconds)
	}
	if probe.FailureThreshold != 0 {
		check.FailureThreshold = int(probe.FailureThreshold)
	}
	return check, validateCheck(check)
}

func inspectService(resource inspectedResource, application App) error {
	service := corev1.Service{}
	if err := decodeManifest(resource.manifest, &service); err != nil {
		return err
	}
	if resource.name != application.Name || application.Port == 0 || !applicationSelector(service.Spec.Selector, resource) {
		return fmt.Errorf("Service must select the matching application listener")
	}
	if (service.Spec.Type != "" && service.Spec.Type != corev1.ServiceTypeClusterIP) ||
		service.Spec.ClusterIP == corev1.ClusterIPNone || len(service.Spec.ExternalIPs) != 0 ||
		service.Spec.PublishNotReadyAddresses || (service.Spec.SessionAffinity != "" && service.Spec.SessionAffinity != corev1.ServiceAffinityNone) {
		return fmt.Errorf("only a private ClusterIP Service with default traffic behavior is supported")
	}
	if len(service.Spec.Ports) != 1 {
		return fmt.Errorf("Service must expose exactly one application port")
	}
	port := service.Spec.Ports[0]
	if int(port.Port) != application.Port || (port.Name != "" && port.Name != "http") ||
		(port.Protocol != "" && port.Protocol != corev1.ProtocolTCP) || port.NodePort != 0 || port.AppProtocol != nil {
		return fmt.Errorf("Service port must match the application TCP listener")
	}
	target, err := applicationPort(port.TargetPort, application.Port)
	if err != nil {
		return err
	}
	if target != application.Port {
		return fmt.Errorf("Service target port must match the application listener")
	}
	return nil
}

func inspectIngress(resource inspectedResource, application App) (Ingress, error) {
	ingress := networkingv1.Ingress{}
	if err := decodeManifest(resource.manifest, &ingress); err != nil {
		return Ingress{}, err
	}
	prefix := application.Name + "-"
	if !strings.HasPrefix(resource.name, prefix) || len(resource.name) == len(prefix) || application.Port == 0 {
		return Ingress{}, fmt.Errorf("route name must follow the application name and require its listener")
	}
	if ingress.Spec.IngressClassName == nil || *ingress.Spec.IngressClassName != "traefik" ||
		ingress.Spec.DefaultBackend != nil || len(ingress.Spec.Rules) != 1 {
		return Ingress{}, fmt.Errorf("only one explicit Traefik HTTP route per Ingress is supported")
	}
	rule := ingress.Spec.Rules[0]
	if rule.HTTP == nil || len(rule.HTTP.Paths) != 1 {
		return Ingress{}, fmt.Errorf("Ingress must contain exactly one HTTP path")
	}
	path := rule.HTTP.Paths[0]
	if path.PathType == nil || *path.PathType != networkingv1.PathTypePrefix || path.Backend.Resource != nil ||
		path.Backend.Service == nil || path.Backend.Service.Name != application.Name ||
		path.Backend.Service.Port.Name != "" || int(path.Backend.Service.Port.Number) != application.Port {
		return Ingress{}, fmt.Errorf("Ingress must route an HTTP prefix to the matching application Service port")
	}
	route := Ingress{Name: strings.TrimPrefix(resource.name, prefix), Host: rule.Host, Path: path.Path}
	annotations := ingress.Annotations
	issuer, err := normalizeCertificateIssuer(annotations["cert-manager.io/cluster-issuer"])
	if err != nil || issuer != annotations["cert-manager.io/cluster-issuer"] ||
		annotations["external-dns.kubernetes.io/hostname"] != rule.Host ||
		annotations["traefik.ingress.kubernetes.io/router.entrypoints"] != "websecure" ||
		annotations["traefik.ingress.kubernetes.io/router.tls"] != "true" {
		return Ingress{}, fmt.Errorf("Ingress must use automatic DNS and certificates on the Traefik HTTPS entrypoint")
	}
	if len(ingress.Spec.TLS) != 1 || len(ingress.Spec.TLS[0].Hosts) != 1 ||
		ingress.Spec.TLS[0].Hosts[0] != rule.Host || ingress.Spec.TLS[0].SecretName != ingressSecretName(resource.name) {
		return Ingress{}, fmt.Errorf("Ingress TLS must use the managed certificate Secret for the route host")
	}
	return route, nil
}

func applicationPort(port intstr.IntOrString, listener int) (int, error) {
	if port.Type == intstr.Int {
		return int(port.IntVal), nil
	}
	if port.Type == intstr.String && port.StrVal == "http" && listener > 0 {
		return listener, nil
	}
	return 0, fmt.Errorf("only numeric ports or the application's named http port are supported")
}

func applicationSelector(selector map[string]string, resource inspectedResource) bool {
	return len(selector) == 2 && selector["app.kubernetes.io/name"] == resource.app && selector["borq.run/target-id"] == resource.target
}

// Decode through the Kubernetes structs to validate field types and integer
// ranges without retaining SDK models in the application's public API.
func decodeManifest(manifest map[string]any, destination any) error {
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("encode resource: %w", err)
	}
	if err := json.Unmarshal(encoded, destination); err != nil {
		return fmt.Errorf("decode resource: %w", err)
	}
	return nil
}
