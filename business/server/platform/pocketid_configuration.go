package platform

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

type pocketIdOverrides struct {
	Resources         corev1.ResourceRequirements   `json:"resources"`
	NodeSelector      map[string]string             `json:"nodeSelector"`
	Tolerations       []corev1.Toleration           `json:"tolerations"`
	Affinity          corev1.Affinity               `json:"affinity"`
	PriorityClassName string                        `json:"priorityClassName"`
	ImagePullSecrets  []corev1.LocalObjectReference `json:"imagePullSecrets"`
	PodAnnotations    map[string]string             `json:"podAnnotations"`
	PodLabels         map[string]string             `json:"podLabels"`
	Image             struct {
		PullPolicy corev1.PullPolicy `json:"pullPolicy"`
	} `json:"image"`
	Config map[string]any `json:"config"`
}

// ValidatePocketIdValues rejects unsupported or unsafe legacy overrides before
// callers persist a provisioning request. Diagnostics never include raw values.
func ValidatePocketIdValues(values map[string]any) error {
	input := PocketIdInput{InstanceID: strings.Repeat("0", 32), Namespace: ManagementNamespace,
		IssuerURL: "https://pocket-id.example.test", Values: values}
	_, err := pocketIdPrivateResources(input)
	return err
}

// PocketIdConfiguration returns the validated editable environment, with explicit
// settings overriding accepted legacy values. The returned map is independent of
// both inputs and excludes Borq's runtime defaults and generated credentials.
func PocketIdConfiguration(values map[string]any, environment map[string]string) (map[string]string, error) {
	overrides, err := pocketIdValues(PocketIdInput{Values: values})
	if err != nil {
		return nil, err
	}
	configuration, err := pocketIdLegacyEnvironment(overrides.Config)
	if err != nil {
		return nil, err
	}
	maps.Copy(configuration, environment)
	if err := ValidatePocketIdEnvironment(configuration); err != nil {
		return nil, err
	}
	return configuration, nil
}

func pocketIdValues(input PocketIdInput) (pocketIdOverrides, error) {
	encoded, err := json.Marshal(input.Values)
	if err != nil || len(encoded) > 128*1024 {
		return pocketIdOverrides{}, errors.New("Pocket ID legacy values must be a JSON-compatible object of at most 128 KiB")
	}
	var supplied map[string]any
	if err := json.Unmarshal(encoded, &supplied); err != nil {
		return pocketIdOverrides{}, errors.New("Pocket ID legacy values must be a JSON-compatible object")
	}
	if pocketIdTemplateExpression(supplied) {
		return pocketIdOverrides{}, errors.New("Pocket ID legacy values cannot contain template expressions")
	}
	var overrides pocketIdOverrides
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&overrides); err != nil {
		return pocketIdOverrides{}, errors.New("Pocket ID legacy values contain unsupported keys or invalid field types")
	}
	if err := validatePocketIdMetadata(overrides.PodLabels, true); err != nil {
		return pocketIdOverrides{}, err
	}
	if err := validatePocketIdMetadata(overrides.PodAnnotations, false); err != nil {
		return pocketIdOverrides{}, err
	}
	for key, value := range overrides.NodeSelector {
		if len(validation.IsQualifiedName(key)) != 0 || len(validation.IsValidLabelValue(value)) != 0 {
			return pocketIdOverrides{}, errors.New("Pocket ID nodeSelector requires valid Kubernetes labels")
		}
	}
	if overrides.PriorityClassName != "" && len(validation.IsDNS1123Subdomain(overrides.PriorityClassName)) != 0 {
		return pocketIdOverrides{}, errors.New("Pocket ID priorityClassName must be a Kubernetes resource name")
	}
	for _, secret := range overrides.ImagePullSecrets {
		if len(validation.IsDNS1123Subdomain(secret.Name)) != 0 {
			return pocketIdOverrides{}, errors.New("Pocket ID imagePullSecrets require Kubernetes Secret names")
		}
	}
	if policy := overrides.Image.PullPolicy; policy != "" && policy != corev1.PullAlways && policy != corev1.PullIfNotPresent && policy != corev1.PullNever {
		return pocketIdOverrides{}, errors.New("Pocket ID image.pullPolicy must be Always, IfNotPresent or Never")
	}
	for _, quantities := range []corev1.ResourceList{overrides.Resources.Requests, overrides.Resources.Limits} {
		for _, quantity := range quantities {
			if quantity.Sign() < 0 {
				return pocketIdOverrides{}, errors.New("Pocket ID resource quantities cannot be negative")
			}
		}
	}
	resources := pocketIdDefaultResources()
	maps.Copy(resources.Requests, overrides.Resources.Requests)
	maps.Copy(resources.Limits, overrides.Resources.Limits)
	for name, request := range resources.Requests {
		if limit, exists := resources.Limits[name]; exists && limit.Cmp(request) < 0 {
			return pocketIdOverrides{}, errors.New("Pocket ID resource limits cannot be smaller than requests")
		}
	}
	overrides.Resources = resources
	return overrides, nil
}

func pocketIdTemplateExpression(value any) bool {
	switch value := value.(type) {
	case string:
		return strings.Contains(value, "{{") || strings.Contains(value, "}}")
	case map[string]any:
		for key, item := range value {
			if pocketIdTemplateExpression(key) || pocketIdTemplateExpression(item) {
				return true
			}
		}
	case []any:
		for _, item := range value {
			if pocketIdTemplateExpression(item) {
				return true
			}
		}
	}
	return false
}

func pocketIdLegacyEnvironment(config map[string]any) (map[string]string, error) {
	names := map[string]string{
		"appName": "APP_NAME", "sessionDuration": "SESSION_DURATION", "homePageUrl": "HOME_PAGE_URL",
		"accentColor": "ACCENT_COLOR", "disableAnimations": "DISABLE_ANIMATIONS", "allowOwnAccountEdit": "ALLOW_OWN_ACCOUNT_EDIT", "requireUserEmail": "REQUIRE_USER_EMAIL",
		"webauthnUserVerification": "WEBAUTHN_USER_VERIFICATION", "webauthnAllowSyncedPasskeys": "WEBAUTHN_ALLOW_SYNCED_PASSKEYS", "webauthnAuthenticatorAttachment": "WEBAUTHN_AUTHENTICATOR_ATTACHMENT",
		"smtpHost": "SMTP_HOST", "smtpPort": "SMTP_PORT", "smtpFrom": "SMTP_FROM", "smtpUser": "SMTP_USER", "smtpPassword": "SMTP_PASSWORD", "smtpTls": "SMTP_TLS",
		"emailLoginNotificationEnabled": "EMAIL_LOGIN_NOTIFICATION_ENABLED", "emailOneTimeAccessAsAdminEnabled": "EMAIL_ONE_TIME_ACCESS_AS_ADMIN_ENABLED",
		"emailApiKeyExpirationEnabled": "EMAIL_API_KEY_EXPIRATION_ENABLED", "emailVerificationEnabled": "EMAIL_VERIFICATION_ENABLED",
	}
	environment := map[string]string{}
	for key, value := range config {
		name, allowed := names[key]
		if !allowed {
			return nil, errors.New("Pocket ID config contains an unsupported or managed setting")
		}
		switch value := value.(type) {
		case string:
			environment[name] = value
		case bool, float64:
			environment[name] = fmt.Sprint(value)
		default:
			return nil, errors.New("Pocket ID config values must be strings, booleans or numbers")
		}
	}
	return environment, nil
}

func validatePocketIdMetadata(metadata map[string]string, labels bool) error {
	for key, value := range metadata {
		if len(validation.IsQualifiedName(key)) != 0 || (labels && len(validation.IsValidLabelValue(value)) != 0) {
			return errors.New("Pocket ID pod metadata must use valid Kubernetes keys and values")
		}
		domain, _, qualified := strings.Cut(key, "/")
		// Admission controller annotations can inject containers or credentials.
		// Keep managed identity and known controller namespaces outside raw values.
		if !qualified {
			return errors.New("Pocket ID custom pod metadata must use an application-owned domain")
		}
		for _, reserved := range []string{"borq", "kubernetes", "k8s", "helm", "istio", "linkerd", "consul", "vault", "dapr", "kuma", "appmesh", "sidecar", "admission", "workload.identity", "opentelemetry", "dynatrace", "newrelic"} {
			if strings.Contains(strings.ToLower(domain), reserved) {
				return errors.New("Pocket ID pod metadata cannot change managed or admission-controller settings")
			}
		}
	}
	return nil
}

func pocketIdPrivateResources(input PocketIdInput) ([]ctrlclient.Object, error) {
	overrides, err := pocketIdValues(input)
	if err != nil {
		return nil, err
	}
	environment, err := PocketIdConfiguration(input.Values, input.Environment)
	if err != nil {
		return nil, err
	}
	if input.Bootstrapping {
		// LDAP imports run independently of ingress, and normal signup is a
		// separate endpoint from first-admin setup. Neither may create a user
		// before Borq proves ownership of the initial administrator.
		environment["LDAP_ENABLED"] = "false"
		environment["ALLOW_USER_SIGNUPS"] = "disabled"
	}
	resources := pocketIdManagedResources(input)
	deployment := resources[2].(*appsv1.Deployment)
	pod := &deployment.Spec.Template
	pod.Labels = maps.Clone(pod.Labels)
	maps.Copy(pod.Labels, overrides.PodLabels)
	pod.Annotations = maps.Clone(overrides.PodAnnotations)
	pod.Spec.NodeSelector, pod.Spec.Tolerations = overrides.NodeSelector, overrides.Tolerations
	if overrides.Affinity.NodeAffinity != nil || overrides.Affinity.PodAffinity != nil || overrides.Affinity.PodAntiAffinity != nil {
		pod.Spec.Affinity = &overrides.Affinity
	}
	pod.Spec.PriorityClassName, pod.Spec.ImagePullSecrets = overrides.PriorityClassName, overrides.ImagePullSecrets
	container := &pod.Spec.Containers[0]
	container.ImagePullPolicy = overrides.Image.PullPolicy
	if container.ImagePullPolicy == "" {
		container.ImagePullPolicy = corev1.PullIfNotPresent
	}
	container.Resources = overrides.Resources
	// Default numeric probe fields explicitly for Kubernetes default-tolerant comparisons.
	container.StartupProbe.SuccessThreshold = 1
	container.ReadinessProbe.SuccessThreshold, container.ReadinessProbe.FailureThreshold = 1, 3
	container.LivenessProbe.SuccessThreshold, container.LivenessProbe.FailureThreshold = 1, 3
	if len(environment) != 0 {
		metadata := pocketIdMetadata(input)
		metadata.Name += "-config"
		secret := &corev1.Secret{ObjectMeta: metadata, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{}}
		for key, value := range environment {
			secret.Data[key] = []byte(value)
		}
		container.EnvFrom = []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: secret.Name}}}}
		encoded, _ := json.Marshal(secret.Data)
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		pod.Annotations["borq.io/pocket-id-config"] = fmt.Sprintf("%x", sha256.Sum256(encoded))
		resources = append(resources, secret)
	}
	definition, _ := json.Marshal(deployment.Spec)
	deployment.Annotations["borq.io/pocket-id-definition"] = fmt.Sprintf("%x", sha256.Sum256(definition))
	return resources, nil
}
