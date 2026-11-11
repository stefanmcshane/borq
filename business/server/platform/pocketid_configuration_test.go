package platform

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestPocketIdLegacyValues proves accepted legacy values and direct environment reach the native workload and
// private environment without YAML injection, public secret disclosure or mutation.
func TestPocketIdLegacyValues(t *testing.T) {
	is := is.New(t)
	input, _, err := normalizePocketId(pocketIdTestInput(t))
	is.NoErr(err)
	password := "secret: value\n---\nkind: Service\nmetadata: {name: injected}\n"
	input.Values = map[string]any{
		"resources":      map[string]any{"requests": map[string]any{"cpu": "75m"}, "limits": map[string]any{"memory": "768Mi"}},
		"nodeSelector":   map[string]any{"kubernetes.io/os": "linux"},
		"podAnnotations": map[string]any{"example.test/team": "identity"},
		"podLabels":      map[string]any{"example.test/component": "login"},
		"image":          map[string]any{"pullPolicy": "Always"},
		"config":         map[string]any{"appName": "Example Identity", "smtpPassword": password, "smtpPort": 587, "disableAnimations": true},
	}
	input.Environment = map[string]string{"APP_NAME": "Group Identity", "LDAP_BIND_PASSWORD": "literal {{ braces }}\npassword: secret"}
	is.NoErr(ValidatePocketIdEnvironment(input.Environment))
	original, err := json.Marshal(input.Values)
	is.NoErr(err)
	is.NoErr(ValidatePocketIdValues(input.Values))
	resources, err := pocketIdPrivateResources(input)
	is.NoErr(err)
	after, err := json.Marshal(input.Values)
	is.NoErr(err)
	is.Equal(string(after), string(original))
	is.Equal(len(resources), 8)
	deployment := resources[2].(*appsv1.Deployment)
	pod := deployment.Spec.Template
	is.Equal(pod.Annotations["example.test/team"], "identity")
	is.Equal(pod.Labels["example.test/component"], "login")
	is.Equal(pod.Spec.NodeSelector["kubernetes.io/os"], "linux")
	is.Equal(pod.Spec.Containers[0].Resources.Requests.Cpu().String(), "75m")
	is.Equal(pod.Spec.Containers[0].Resources.Limits.Memory().String(), "768Mi")
	is.Equal(pod.Spec.Containers[0].ImagePullPolicy, corev1.PullAlways)
	secret := resources[7].(*corev1.Secret)
	is.Equal(secret.Name, "pocketid-"+input.InstanceID+"-config")
	is.Equal(string(secret.Data["SMTP_PASSWORD"]), password)
	is.Equal(string(secret.Data["SMTP_PORT"]), "587")
	is.Equal(string(secret.Data["DISABLE_ANIMATIONS"]), "true")
	is.Equal(string(secret.Data["APP_NAME"]), "Group Identity")
	is.Equal(string(secret.Data["LDAP_BIND_PASSWORD"]), input.Environment["LDAP_BIND_PASSWORD"])
	is.Equal(pod.Spec.Containers[0].EnvFrom[0].SecretRef.Name, secret.Name)
	for _, object := range resources[:7] {
		encoded, err := json.Marshal(object)
		is.NoErr(err)
		is.True(!strings.Contains(string(encoded), "Example Identity"))
		is.True(!strings.Contains(string(encoded), "secret: value"))
	}
}

// TestPocketIdRejectsUnsafeValues owns raw-value admission, including
// alternate credential sources and settings which would bypass the signup guard.
func TestPocketIdRejectsUnsafeValues(t *testing.T) {
	for name, values := range map[string]map[string]any{
		"image replacement":          {"image": map[string]any{"repository": "do-not-print"}},
		"public service":             {"service": map[string]any{"type": "LoadBalancer"}},
		"ingress before admin":       {"ingress": map[string]any{"enabled": true}},
		"storage replacement":        {"persistence": map[string]any{"existingClaim": "do-not-print"}},
		"privileged container":       {"securityContext": map[string]any{"privileged": true}},
		"host volume":                {"volumes": []any{map[string]any{"hostPath": map[string]any{"path": "/"}}}},
		"service account":            {"serviceAccount": map[string]any{"automount": true}},
		"unknown environment secret": {"existingSecret": "do-not-print"},
		"database path":              {"config": map[string]any{"dbConnectionString": "do-not-print"}},
		"public listener":            {"config": map[string]any{"host": "0.0.0.0"}},
		"alternate encryption file":  {"config": map[string]any{"encryptionKeyFile": "do-not-print"}},
		"alternate static key":       {"config": map[string]any{"staticApiKey": "do-not-print"}},
		"application origin":         {"config": map[string]any{"appUrl": "https://do-not-print.test"}},
		"template execution":         {"config": map[string]any{"smtpPassword": "{{ .Values.do_not_print }}"}},
		"telemetry exfiltration":     {"config": map[string]any{"otelLogsExporter": "otlp"}},
		"controller injection":       {"podAnnotations": map[string]any{"vault.hashicorp.com/agent-inject": "true"}},
		"identity labels":            {"podLabels": map[string]any{"borq.io/pocket-id": "do-not-print"}},
		"reserved hash":              {"podAnnotations": map[string]any{"borq.io/pocket-id-config": "do-not-print"}},
		"unsafe field type":          {"resources": "do-not-print"},
		"negative resources":         {"resources": map[string]any{"requests": map[string]any{"cpu": "-1"}}},
		"invalid pull policy":        {"image": map[string]any{"pullPolicy": "do-not-print"}},
		"invalid priority class":     {"priorityClassName": "BAD/CLASS"},
		"invalid config object":      {"config": map[string]any{"smtpPassword": map[string]any{"private": "do-not-print"}}},
	} {
		t.Run(name, func(t *testing.T) {
			is := is.New(t)
			err := ValidatePocketIdValues(values)
			is.True(err != nil)
			is.True(strings.Contains(err.Error(), "Pocket ID"))
			is.True(!strings.Contains(err.Error(), "do-not-print"))
		})
	}
}

// TestPocketIdConfigurationUpdates covers removals as well as additions: Kubernetes
// default-tolerant comparison must not retain a removed environment or selector.
func TestPocketIdConfigurationUpdates(t *testing.T) {
	is := is.New(t)
	input, _, err := normalizePocketId(pocketIdTestInput(t))
	is.NoErr(err)
	input.Values = map[string]any{"nodeSelector": map[string]any{"example.test/tier": "identity"}, "config": map[string]any{"appName": "First"}}
	initial, err := pocketIdPrivateResources(input)
	is.NoErr(err)
	client := fake.NewClientBuilder().WithScheme(kubernetes.NewScheme()).WithObjects(initial...).Build()
	input.Values = map[string]any{"config": map[string]any{"appName": "Second"}}
	changed, err := pocketIdPrivateResources(input)
	is.NoErr(err)
	for _, index := range []int{7, 2} {
		is.NoErr(ensurePocketIdResource(context.Background(), client, changed[index]))
	}
	var deployment appsv1.Deployment
	is.NoErr(client.Get(context.Background(), ctrlclient.ObjectKeyFromObject(changed[2]), &deployment))
	is.Equal(len(deployment.Spec.Template.Spec.NodeSelector), 0)
	var config corev1.Secret
	is.NoErr(client.Get(context.Background(), ctrlclient.ObjectKeyFromObject(changed[7]), &config))
	is.Equal(string(config.Data["APP_NAME"]), "Second")
	input.Values = nil
	removed, err := pocketIdPrivateResources(input)
	is.NoErr(err)
	is.NoErr(ensurePocketIdResource(context.Background(), client, removed[2]))
	is.NoErr(client.Get(context.Background(), ctrlclient.ObjectKeyFromObject(removed[2]), &deployment))
	is.Equal(len(deployment.Spec.Template.Spec.Containers[0].EnvFrom), 0)
	var auth corev1.Secret
	is.NoErr(client.Get(context.Background(), ctrlclient.ObjectKeyFromObject(initial[0]), &auth))
	is.Equal(string(auth.Data["encryption-key"]), input.EncryptionKey)
}

// TestPocketIdBootstrapEnvironment prevents directory imports or ordinary
// signup from racing the initial administrator, then restores accepted settings.
func TestPocketIdBootstrapEnvironment(t *testing.T) {
	is := is.New(t)
	input, _, err := normalizePocketId(pocketIdTestInput(t))
	is.NoErr(err)
	input.Environment = map[string]string{"LDAP_ENABLED": "true", "ALLOW_USER_SIGNUPS": "open", "APP_NAME": "Managed Identity"}
	input.Bootstrapping = true
	resources, err := pocketIdPrivateResources(input)
	is.NoErr(err)
	initial := resources[7].(*corev1.Secret)
	is.Equal(string(initial.Data["LDAP_ENABLED"]), "false")
	is.Equal(string(initial.Data["ALLOW_USER_SIGNUPS"]), "disabled")
	is.Equal(string(initial.Data["APP_NAME"]), "Managed Identity")
	input.Bootstrapping = false
	resources, err = pocketIdPrivateResources(input)
	is.NoErr(err)
	ready := resources[7].(*corev1.Secret)
	is.Equal(string(ready.Data["LDAP_ENABLED"]), "true")
	is.Equal(string(ready.Data["ALLOW_USER_SIGNUPS"]), "open")
	is.Equal(input.Environment["LDAP_ENABLED"], "true")
}

// TestPocketIdRepairsBoundaryDrift proves a retained definition annotation
// cannot conceal extra listeners, credentials or containers on a live workload.
func TestPocketIdRepairsBoundaryDrift(t *testing.T) {
	for name, mutate := range map[string]func(*appsv1.Deployment){
		"extra credential source": func(deployment *appsv1.Deployment) {
			deployment.Spec.Template.Spec.Containers[0].EnvFrom = []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "foreign"}}}}
		},
		"duplicate listener override": func(deployment *appsv1.Deployment) {
			deployment.Spec.Template.Spec.Containers[0].Env = append(deployment.Spec.Template.Spec.Containers[0].Env, corev1.EnvVar{Name: "HOST", Value: "0.0.0.0"})
		},
		"additional container": func(deployment *appsv1.Deployment) {
			deployment.Spec.Template.Spec.Containers = append(deployment.Spec.Template.Spec.Containers, corev1.Container{Name: "unguarded", Image: "example.test/extra"})
		},
		"service account": func(deployment *appsv1.Deployment) { deployment.Spec.Template.Spec.ServiceAccountName = "foreign" },
	} {
		t.Run(name, func(t *testing.T) {
			is := is.New(t)
			input, _, err := normalizePocketId(pocketIdTestInput(t))
			is.NoErr(err)
			resources, err := pocketIdPrivateResources(input)
			is.NoErr(err)
			wanted := resources[2].(*appsv1.Deployment)
			current := wanted.DeepCopy()
			mutate(current)
			client := fake.NewClientBuilder().WithScheme(kubernetes.NewScheme()).WithObjects(current).Build()
			is.NoErr(ensurePocketIdResource(context.Background(), client, wanted))
			is.NoErr(client.Get(context.Background(), ctrlclient.ObjectKeyFromObject(current), current))
			is.Equal(current.Spec, wanted.Spec)
		})
	}
}
