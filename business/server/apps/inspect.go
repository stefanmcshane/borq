package apps

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
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
	claims, backupConfigs := make(map[string]bool), make(map[string]bool)
	for _, resource := range resources {
		if resource.kind == "Deployment" {
			continue
		}
		if resource.kind == "ExternalSecret" && resource.app == "" {
			continue
		}
		application, exists := applications[resource.app]
		if !exists {
			return Configuration{}, fmt.Errorf("%s/%s has no matching application Deployment", resource.kind, resource.name)
		}
		if resource.kind == "ExternalSecret" {
			continue
		}
		if resource.kind == "ServiceAccount" || resource.kind == "Role" || resource.kind == "RoleBinding" || resource.kind == "CronJob" {
			if err := inspectSqliteAuxiliary(resource, application); err != nil {
				return Configuration{}, fmt.Errorf("%s/%s: %w", resource.kind, resource.name, err)
			}
			continue
		}
		if resource.kind == "PersistentVolumeClaim" || resource.kind == "ConfigMap" {
			if err := inspectSqliteResource(resource, application); err != nil {
				return Configuration{}, fmt.Errorf("%s/%s: %w", resource.kind, resource.name, err)
			}
			if resource.kind == "PersistentVolumeClaim" {
				claims[application.Name] = true
			} else if resource.name == application.Name+"-litestream" {
				backupConfigs[application.Name] = true
			}
			continue
		}
		if resource.kind == "Gateway" || resource.kind == "Certificate" {
			found := false
			for _, route := range resources {
				if route.kind == "HTTPRoute" && route.app == resource.app && (route.name == resource.name || resource.kind == "Certificate" && ingressSecretName(route.name) == resource.name) {
					found = true
				}
			}
			if !found {
				return Configuration{}, fmt.Errorf("%s/%s has no matching HTTPRoute", resource.kind, resource.name)
			}
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
		ingress, err := inspectHttpRoute(resource, application, resources)
		if err != nil {
			return Configuration{}, fmt.Errorf("HTTPRoute/%s: %w", resource.name, err)
		}
		application.Ingresses = append(application.Ingresses, ingress)
		applications[application.Name] = application
	}
	configuration := Configuration{Apps: make([]App, 0, len(applications))}
	for _, application := range applications {
		if application.Sqlite.Enabled && !claims[application.Name] || application.Sqlite.Backups && !backupConfigs[application.Name] {
			return Configuration{}, fmt.Errorf("app %q has missing SQLite storage or backup configuration", application.Name)
		}
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
			(kind == "Service" && version == "v1") || ((kind == "HTTPRoute" || kind == "Gateway") && version == "gateway.networking.k8s.io/v1") || (kind == "Certificate" && version == "cert-manager.io/v1") ||
			(kind == "Job" && version == "batch/v1") || (kind == "ExternalSecret" && version == "external-secrets.io/v1") ||
			((kind == "PersistentVolumeClaim" || kind == "ConfigMap" || kind == "ServiceAccount") && version == "v1") ||
			(kind == "Role" || kind == "RoleBinding") && version == "rbac.authorization.k8s.io/v1" || kind == "CronJob" && version == "batch/v1") {
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
			(len(validation.IsDNS1035Label(app)) > 0 && !(kind == "ExternalSecret" && envelope.Labels["borq.run/phase"] == "registry" && app == "")) {
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
	settings, variables, err := inspectSqlite(deployment.Spec.Template.Spec, container, true)
	if err != nil {
		return App{}, err
	}
	application.Sqlite = settings
	if settings.Enabled && deployment.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		return App{}, fmt.Errorf("SQLite requires a Recreate deployment strategy")
	}
	application.Env, err = inspectEnvironmentReference(variables, container.EnvFrom, environment, cmp.Or(deployment.Annotations["borq.run/environment-group"], deployment.Annotations["borq.run/env-config"]))
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

func inspectHttpRoute(resource inspectedResource, application App, resources []inspectedResource) (Ingress, error) {
	var route gatewayv1.HTTPRoute
	if err := decodeManifest(resource.manifest, &route); err != nil {
		return Ingress{}, err
	}
	prefix := application.Name + "-"
	if !strings.HasPrefix(resource.name, prefix) || len(resource.name) == len(prefix) || application.Port == 0 {
		return Ingress{}, fmt.Errorf("route name must follow the application name and require its listener")
	}
	if len(route.Spec.Hostnames) != 1 || len(route.Spec.Rules) != 1 || len(route.Spec.ParentRefs) != 1 {
		return Ingress{}, fmt.Errorf("HTTPRoute requires one hostname, rule and managed Gateway parent")
	}
	parent := route.Spec.ParentRefs[0]
	if string(parent.Name) != route.Name || parent.Namespace != nil && string(*parent.Namespace) != route.Namespace || parent.Group != nil && *parent.Group != gatewayv1.GroupName || parent.Kind != nil && *parent.Kind != "Gateway" || parent.SectionName == nil || (*parent.SectionName != "websecure" && *parent.SectionName != "web") || parent.Port != nil {
		return Ingress{}, fmt.Errorf("HTTPRoute must bind its managed HTTPS or tunnel listener")
	}
	rule := route.Spec.Rules[0]
	if len(rule.Matches) != 1 || len(rule.BackendRefs) != 1 || *parent.SectionName == "websecure" && len(rule.Filters) != 0 || rule.Timeouts != nil || rule.Retry != nil || rule.SessionPersistence != nil {
		return Ingress{}, fmt.Errorf("HTTPRoute requires one HTTP prefix and backend")
	}
	match, backend := rule.Matches[0], rule.BackendRefs[0]
	if match.Path == nil || match.Path.Type == nil || *match.Path.Type != gatewayv1.PathMatchPathPrefix || match.Path.Value == nil || match.Method != nil || len(match.Headers) != 0 || len(match.QueryParams) != 0 ||
		string(backend.Name) != application.Name || backend.Port == nil || int(*backend.Port) != application.Port || backend.Namespace != nil && string(*backend.Namespace) != route.Namespace || backend.Group != nil && *backend.Group != "" || backend.Kind != nil && *backend.Kind != "Service" || backend.Weight != nil && *backend.Weight != 1 || len(backend.Filters) != 0 {
		return Ingress{}, fmt.Errorf("HTTPRoute must route an HTTP prefix to the matching application Service port")
	}
	host := string(route.Spec.Hostnames[0])
	if route.Annotations["external-dns.kubernetes.io/hostname"] != host {
		return Ingress{}, fmt.Errorf("HTTPRoute DNS annotation must match its hostname")
	}
	var gateway gatewayv1.Gateway
	var certificate certmanagerv1.Certificate
	for _, auxiliary := range resources {
		if auxiliary.app != application.Name {
			continue
		}
		if auxiliary.kind == "Gateway" && auxiliary.name == route.Name {
			if err := decodeManifest(auxiliary.manifest, &gateway); err != nil {
				return Ingress{}, err
			}
		}
		if auxiliary.kind == "Certificate" && auxiliary.name == ingressSecretName(route.Name) {
			if err := decodeManifest(auxiliary.manifest, &certificate); err != nil {
				return Ingress{}, err
			}
		}
	}
	if gateway.Name == "" || gateway.Spec.GatewayClassName != "borq-traefik" || len(gateway.Spec.Listeners) != 1 {
		return Ingress{}, fmt.Errorf("HTTPRoute requires its Borq Gateway")
	}
	listener := gateway.Spec.Listeners[0]
	if listener.AllowedRoutes != nil && (listener.AllowedRoutes.Namespaces != nil && (listener.AllowedRoutes.Namespaces.From != nil && *listener.AllowedRoutes.Namespaces.From != gatewayv1.NamespacesFromSame || listener.AllowedRoutes.Namespaces.Selector != nil) || len(listener.AllowedRoutes.Kinds) != 0) {
		return Ingress{}, fmt.Errorf("Gateway must restrict routes to its namespace")
	}
	if *parent.SectionName == "web" {
		if !applicationTunnelRoute(route, gateway) || certificate.Name != "" {
			return Ingress{}, fmt.Errorf("tunnel HTTPRoute requires its managed HTTP listener and HTTPS forwarding header without a Certificate")
		}
		return Ingress{Name: strings.TrimPrefix(resource.name, prefix), Host: host, Path: *match.Path.Value}, nil
	}
	if listener.Name != "websecure" || listener.Protocol != gatewayv1.HTTPSProtocolType || listener.Port != 8443 || listener.Hostname == nil || string(*listener.Hostname) != host || listener.TLS == nil || len(listener.TLS.CertificateRefs) != 1 || listener.TLS.Mode != nil && *listener.TLS.Mode != gatewayv1.TLSModeTerminate {
		return Ingress{}, fmt.Errorf("Gateway must terminate HTTPS for the HTTPRoute hostname")
	}
	ref := listener.TLS.CertificateRefs[0]
	if string(ref.Name) != ingressSecretName(route.Name) || ref.Namespace != nil && string(*ref.Namespace) != route.Namespace || ref.Group != nil && *ref.Group != "" || ref.Kind != nil && *ref.Kind != "Secret" {
		return Ingress{}, fmt.Errorf("Gateway must use the managed certificate Secret")
	}
	issuer, err := normalizeCertificateIssuer(certificate.Spec.IssuerRef.Name)
	if err != nil || issuer != certificate.Spec.IssuerRef.Name || certificate.Spec.IssuerRef.Kind != "ClusterIssuer" || certificate.Spec.IssuerRef.Group != "" && certificate.Spec.IssuerRef.Group != "cert-manager.io" || len(certificate.Spec.DNSNames) != 1 || certificate.Spec.DNSNames[0] != host || certificate.Spec.SecretName != ingressSecretName(route.Name) {
		return Ingress{}, fmt.Errorf("HTTPRoute requires its managed Certificate for the hostname")
	}
	return Ingress{Name: strings.TrimPrefix(resource.name, prefix), Host: host, Path: *match.Path.Value}, nil
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

// applicationTunnelRoute accepts only Borq's private origin listener and the
// single forwarding header needed after Cloudflare terminates public HTTPS.
func applicationTunnelRoute(route gatewayv1.HTTPRoute, gateway gatewayv1.Gateway) bool {
	if len(route.Spec.ParentRefs) != 1 || route.Spec.ParentRefs[0].SectionName == nil || *route.Spec.ParentRefs[0].SectionName != "web" || len(route.Spec.Rules) != 1 || len(route.Spec.Hostnames) != 1 || len(gateway.Spec.Listeners) != 1 {
		return false
	}
	listener := gateway.Spec.Listeners[0]
	if listener.Name != "web" || listener.Port != 8000 || listener.Protocol != gatewayv1.HTTPProtocolType || listener.TLS != nil || listener.Hostname == nil || *listener.Hostname != route.Spec.Hostnames[0] {
		return false
	}
	wanted := []gatewayv1.HTTPRouteFilter{{Type: gatewayv1.HTTPRouteFilterRequestHeaderModifier, RequestHeaderModifier: &gatewayv1.HTTPHeaderFilter{Set: []gatewayv1.HTTPHeader{{Name: "X-Forwarded-Proto", Value: "https"}}}}}
	return reflect.DeepEqual(route.Spec.Rules[0].Filters, wanted)
}
