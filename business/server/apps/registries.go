package apps

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/kubeapps"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	"github.com/stefanmcshane/borq/foundations/openbao"
	"github.com/stefanmcshane/borq/foundations/sqlite"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	kubeclient "k8s.io/client-go/kubernetes"
)

// RegistryAuth accepts Docker/OCI registry credentials or reuses a named, scope-bound OpenBao reference.
type RegistryAuth struct {
	// Server is the registry address, independent of the secret name. Omitted only for legacy ghcr.io credentials or existing references.
	Server string `json:"server,omitempty"`
	// Username is the registry account; accepted only at input.
	Username string `json:"username,omitempty"`
	// Token is staged into OpenBao before configuration or queued work is persisted.
	Token string `json:"token,omitempty"`
	// CredentialsRef selects an immutable credential document belonging to this scope.
	CredentialsRef string `json:"credentialsRef,omitempty"`
}

func normalizeRegistryServer(address string) (string, error) {
	raw := strings.TrimSpace(address)
	if raw == "" || strings.ContainsAny(raw, "\r\n\t ") {
		return "", fmt.Errorf("%w: enter a registry host or HTTP(S) URL", ErrInvalid)
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Opaque != "" {
		return "", fmt.Errorf("%w: registry address must be a host or HTTP(S) URL without credentials, query, or fragment", ErrInvalid)
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" || strings.ContainsAny(host, "*%\\") || strings.HasSuffix(parsed.Host, ":") {
		return "", fmt.Errorf("%w: invalid registry host", ErrInvalid)
	}
	if net.ParseIP(host) == nil {
		if err := validateConfigName(host); err != nil {
			return "", fmt.Errorf("%w: invalid registry host", ErrInvalid)
		}
	}
	if port := parsed.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return "", fmt.Errorf("%w: invalid registry port", ErrInvalid)
		}
	}
	registryPath := parsed.EscapedPath()
	if registryPath == "/" {
		registryPath = ""
	}
	if strings.ContainsAny(registryPath, "%*\\") {
		return "", fmt.Errorf("%w: invalid registry path", ErrInvalid)
	}
	for _, part := range strings.Split(registryPath, "/") {
		if part == "." || part == ".." {
			return "", fmt.Errorf("%w: invalid registry path", ErrInvalid)
		}
	}
	// Docker Hub's historical config key is also understood by Kubernetes.
	if parsed.Port() == "" && (host == "docker.io" || host == "index.docker.io") && (registryPath == "" || registryPath == "/v1/" || registryPath == "/v2/") {
		return "https://index.docker.io/v1/", nil
	}
	authority := strings.ToLower(parsed.Host)
	if registryPath != "" {
		// Trailing delimiters and API prefixes affect kubelet credential matching.
		authority += registryPath
	}
	return authority, nil
}

func registryServer(name string, auth RegistryAuth) (string, error) {
	if auth.Server != "" {
		return normalizeRegistryServer(auth.Server)
	}
	if auth.CredentialsRef != "" {
		return "", nil
	}
	if name == "ghcr.io" {
		return "ghcr.io", nil
	}
	return "", fmt.Errorf("%w: registry server is required separately from the secret name", ErrInvalid)
}

func validateRegistryReference(ref, scope string) error {
	prefix := "borq/registry-auth/" + scope + "/"
	id := strings.TrimPrefix(ref, prefix)
	if !strings.HasPrefix(ref, prefix) || len(id) != 32 || strings.Trim(id, "0123456789abcdef") != "" {
		return fmt.Errorf("%w: registry credentialsRef must belong to this scope", ErrInvalid)
	}
	return nil
}

func validateRegistries(registries map[string]RegistryAuth, scope string) error {
	for name, auth := range registries {
		if err := validateConfigName(name); err != nil {
			return err
		}
		if _, err := registryServer(name, auth); err != nil {
			return err
		}
		if auth.CredentialsRef != "" {
			if auth.Username != "" || auth.Token != "" {
				return fmt.Errorf("%w: registry credentialsRef cannot accompany inline credentials", ErrInvalid)
			}
			if err := validateRegistryReference(auth.CredentialsRef, scope); err != nil {
				return err
			}
			continue
		}
		if strings.TrimSpace(auth.Username) == "" || strings.ContainsAny(auth.Username, ":\r\n\x00") || strings.TrimSpace(auth.Token) == "" || strings.ContainsAny(auth.Token, "\r\n\x00") {
			return fmt.Errorf("%w: registry username and password/token are required together", ErrInvalid)
		}
	}
	return nil
}

type dockerRegistryConfig struct {
	Auths map[string]dockerRegistryCredential `json:"auths"`
}

type dockerRegistryCredential struct {
	Auth     string `json:"auth,omitempty"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

func registryEntryValues(entry openbao.KvEntry) (RegistryAuth, error) {
	var config dockerRegistryConfig
	if err := json.Unmarshal([]byte(entry.Values[".dockerconfigjson"]), &config); err != nil || len(config.Auths) != 1 {
		return RegistryAuth{}, errors.New("registry secret is invalid")
	}
	for server, credential := range config.Auths {
		normalized, err := normalizeRegistryServer(server)
		if err != nil {
			return RegistryAuth{}, errors.New("registry secret is invalid")
		}
		username, password := credential.Username, credential.Password
		if credential.Auth != "" {
			decoded, err := base64.StdEncoding.DecodeString(credential.Auth)
			var ok bool
			username, password, ok = strings.Cut(string(decoded), ":")
			if err != nil || !ok {
				return RegistryAuth{}, errors.New("registry secret is invalid")
			}
		}
		if username == "" || password == "" {
			return RegistryAuth{}, errors.New("registry secret is invalid")
		}
		return RegistryAuth{Server: normalized, Username: username, Token: password}, nil
	}
	return RegistryAuth{}, errors.New("registry secret is invalid")
}

func registryEntryMatches(entry openbao.KvEntry, name string) bool {
	if entry.PlanID == "registry/"+name {
		return true
	}
	// Old GHCR documents have no identity tag. Never adopt them under a new name.
	if entry.PlanID == "" && name == "ghcr.io" {
		values, err := registryEntryValues(entry)
		return err == nil && values.Server == "ghcr.io"
	}
	return false
}

func (manager Manager) stageRegistries(ctx context.Context, registries map[string]RegistryAuth, scope string) (map[string]RegistryAuth, error) {
	if err := validateRegistries(registries, scope); err != nil {
		return nil, err
	}
	if len(registries) == 0 {
		return registries, nil
	}
	vault, err := platform.ManagementVault(ctx, manager.runtime.managementKubeconfigPath, manager.runtime.platformSettings.Namespace)
	if err != nil {
		return nil, err
	}
	safe := maps.Clone(registries)
	for name, auth := range registries {
		server, err := registryServer(name, auth)
		if err != nil {
			return nil, err
		}
		if auth.CredentialsRef != "" {
			entry, err := vault.ReadKv(ctx, "secret", auth.CredentialsRef, 1)
			if err != nil {
				return nil, errors.New("registry credentials are unavailable in OpenBao")
			}
			if !registryEntryMatches(entry, name) {
				return nil, fmt.Errorf("%w: registry credentialsRef belongs to another secret", ErrInvalid)
			}
			values, err := registryEntryValues(entry)
			if err != nil {
				return nil, err
			}
			if server != "" && server != values.Server {
				return nil, fmt.Errorf("%w: registry credentialsRef belongs to another registry server", ErrInvalid)
			}
			safe[name] = RegistryAuth{Server: values.Server, CredentialsRef: auth.CredentialsRef}
			continue
		}
		id, err := newId()
		if err != nil {
			return nil, err
		}
		ref := "borq/registry-auth/" + scope + "/" + id
		config, err := json.Marshal(dockerRegistryConfig{Auths: map[string]dockerRegistryCredential{server: {Auth: base64.StdEncoding.EncodeToString([]byte(auth.Username + ":" + auth.Token))}}})
		if err != nil {
			return nil, errors.New("encode registry credentials")
		}
		if _, err := vault.WriteKv(ctx, "secret", ref, openbao.WriteKvInput{Values: map[string]string{".dockerconfigjson": string(config)}, PlanID: "registry/" + name}); err != nil {
			return nil, err
		}
		safe[name] = RegistryAuth{Server: server, CredentialsRef: ref}
	}
	return safe, nil
}

func (manager Manager) readRegistries(ctx context.Context, scope string) (map[string]RegistryAuth, error) {
	registries, _, err := loadDocument[map[string]RegistryAuth](ctx, manager.database, "registry-auth", scope)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return registries, err
}

func (manager Manager) saveRegistries(ctx context.Context, scope string, registries map[string]RegistryAuth) error {
	previous, record, err := loadDocument[map[string]RegistryAuth](ctx, manager.database, "registry-auth", scope)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if previous == nil {
		previous = map[string]RegistryAuth{}
	}
	maps.Copy(previous, registries)
	write, err := documentWrite("registry-auth", scope, previous, record.Revision)
	if err != nil {
		return err
	}
	return manager.database.Commit(ctx, []sqlite.Write{write})
}

func registrySecretName(host string, auth RegistryAuth) string {
	digest := sha256.Sum256([]byte(host + "/" + auth.CredentialsRef))
	return fmt.Sprintf("borq-registry-%x", digest[:12])
}

func registryExternalSecrets(registries map[string]RegistryAuth, namespace, scope, store string) []map[string]any {
	manifests := []map[string]any{}
	for _, host := range slices.Sorted(maps.Keys(registries)) {
		auth := registries[host]
		name := registrySecretName(host, auth)
		labels := map[string]any{"app.kubernetes.io/managed-by": "borq", "borq.run/target-id": scope, "borq.run/phase": "registry"}
		manifests = append(manifests, map[string]any{
			"apiVersion": "external-secrets.io/v1", "kind": "ExternalSecret",
			"metadata": map[string]any{"name": name, "namespace": namespace, "labels": labels, "annotations": map[string]any{"borq.run/registry-secret": host}},
			"spec": map[string]any{
				"refreshPolicy": "OnChange", "secretStoreRef": map[string]any{"kind": "SecretStore", "name": store},
				"dataFrom": []any{map[string]any{"extract": map[string]any{"key": auth.CredentialsRef, "property": "values", "version": "1"}}},
				"target": map[string]any{"name": name, "creationPolicy": "Owner", "deletionPolicy": "Retain", "immutable": true,
					"template": map[string]any{"type": "kubernetes.io/dockerconfigjson", "metadata": map[string]any{"labels": labels}}},
			},
		})
	}
	return manifests
}

func addRegistryPullSecrets(manifests []map[string]any, registries map[string]RegistryAuth) {
	if len(registries) == 0 {
		return
	}
	for _, manifest := range manifests {
		path := []string{"spec", "template", "spec"}
		if manifest["kind"] == "CronJob" {
			path = []string{"spec", "jobTemplate", "spec", "template", "spec"}
		}
		pod, found, _ := unstructured.NestedMap(manifest, path...)
		if !found {
			continue
		}
		existing, _ := pod["imagePullSecrets"].([]any)
		secrets := []any{}
		for _, value := range existing {
			secret, _ := value.(map[string]any)
			name, _ := secret["name"].(string)
			if !strings.HasPrefix(name, "borq-registry-") {
				secrets = append(secrets, value)
			}
		}
		for _, host := range slices.Sorted(maps.Keys(registries)) {
			secrets = append(secrets, map[string]any{"name": registrySecretName(host, registries[host])})
		}
		pod["imagePullSecrets"] = secrets
		_ = unstructured.SetNestedMap(manifest, pod, path...)
	}
}

func preserveUnselectedRegistryPullSecrets(manifests, previous []map[string]any, patch Patch) []map[string]any {
	if patch.Apps == nil {
		return manifests
	}
	selected := map[string]bool{}
	for _, application := range *patch.Apps {
		selected[application.Name] = true
	}
	stored := map[string]map[string]any{}
	existing, needed := map[string]bool{}, map[string]bool{}
	for _, manifest := range previous {
		stored[manifestIdentity(manifest)] = manifest
	}
	for _, manifest := range manifests {
		existing[manifestIdentity(manifest)] = true
		path := []string{"spec", "template", "spec", "imagePullSecrets"}
		if manifest["kind"] == "CronJob" {
			path = []string{"spec", "jobTemplate", "spec", "template", "spec", "imagePullSecrets"}
		}
		old, found := stored[manifestIdentity(manifest)]
		if found && !selected[manifestText(manifest, "metadata", "labels", "app.kubernetes.io/name")] {
			secrets, present, _ := unstructured.NestedSlice(old, path...)
			if present {
				_ = unstructured.SetNestedSlice(manifest, secrets, path...)
			} else {
				unstructured.RemoveNestedField(manifest, path...)
			}
		}
		secrets, _, _ := unstructured.NestedSlice(manifest, path...)
		for _, item := range secrets {
			secret, _ := item.(map[string]any)
			name, _ := secret["name"].(string)
			needed[name] = true
		}
	}
	// A partial app deployment keeps other apps' pinned credentials and their
	// projection resources, so adopting new space credentials cannot roll them.
	for _, manifest := range previous {
		if manifest["kind"] == "ExternalSecret" && manifestText(manifest, "metadata", "labels", "borq.run/phase") == "registry" &&
			needed[manifestText(manifest, "metadata", "name")] && !existing[manifestIdentity(manifest)] {
			manifests = append(manifests, manifest)
		}
	}
	return manifests
}

func (manager Manager) activateSpaceRegistries(ctx context.Context, space Space, registries map[string]RegistryAuth) error {
	manager.runtime.secretMutex.Lock()
	defer manager.runtime.secretMutex.Unlock()
	return manager.activateSpaceRegistriesUnlocked(ctx, space, registries)
}

func (manager Manager) activateSpaceRegistriesUnlocked(ctx context.Context, space Space, registries map[string]RegistryAuth) error {
	for host := range registries {
		if group, _, err := loadDocument[environmentGroupRecord](ctx, manager.database, "environment-group", space.ID+"/"+host); err == nil && !group.Retired {
			return fmt.Errorf("%w: secret already exists with type secret-group", ErrConflict)
		} else if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
	}
	// Staged management actions may outlive deletion of their credential source.
	if _, err := manager.stageRegistries(ctx, registries, space.ID); err != nil {
		return err
	}
	cluster, _, err := loadDocument[Cluster](ctx, manager.database, "cluster", space.Organization+"/"+space.Cluster)
	if err != nil {
		return err
	}
	client, err := newClusterClient(cluster)
	if err != nil {
		return err
	}
	// Registry settings can be saved before the space has any applications.
	if err := client.EnsureNamespace(ctx, kubeapps.NamespaceInput{Name: space.Namespace, TargetID: space.ID}); err != nil {
		return err
	}
	if err := manager.prepareSpaceRegistries(ctx, cluster, space, registries); err != nil {
		return err
	}
	manifests := registryExternalSecrets(registries, space.Namespace, space.ID, "borq-registry-openbao")
	if err := client.Apply(ctx, manifests, space.ID); err != nil {
		return err
	}
	// ESO and kubelet reconcile asynchronously; saving does not wait for a pull.
	return manager.saveRegistries(ctx, space.ID, registries)
}

func (manager Manager) prepareSpaceRegistries(ctx context.Context, cluster Cluster, space Space, registries map[string]RegistryAuth) error {
	if len(registries) == 0 {
		return nil
	}
	manager.runtime.registryMutex.Lock()
	defer manager.runtime.registryMutex.Unlock()
	vault, err := platform.ManagementVault(ctx, manager.runtime.managementKubeconfigPath, manager.runtime.platformSettings.Namespace)
	if err != nil {
		return err
	}
	token, err := ensureSpaceRegistryToken(ctx, vault, space.ID)
	if err != nil {
		return err
	}
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: cluster.KubeconfigPath, Context: cluster.Context, InCluster: cluster.InCluster}, logger.Logger{})
	if err != nil {
		return err
	}
	existing, err := connection.Secrets().Get(ctx, space.Namespace, "borq-registry-openbao")
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if err == nil && existing.Labels["borq.run/target-id"] != space.ID {
		return fmt.Errorf("%w: registry access Secret belongs to another space", ErrConflict)
	}
	if err := connection.Secrets().Apply(ctx, kubernetes.Secret{Name: "borq-registry-openbao", Namespace: space.Namespace, Labels: map[string]string{"app.kubernetes.io/managed-by": "borq", "borq.run/target-id": space.ID}, Data: map[string]string{"token": token}}); err != nil {
		return err
	}
	store, err := registryStore(ctx, connection, space.Namespace, false, manager.runtime.platformSettings.Namespace)
	if err != nil {
		return err
	}
	return connection.Objects().Apply(ctx, store)
}

// The SDK client pointer shares its authenticated HTTP transport across vault requests.
func ensureSpaceRegistryToken(ctx context.Context, vault *openbao.Client, spaceID string) (string, error) {
	if len(spaceID) != 32 || strings.Trim(spaceID, "0123456789abcdef") != "" {
		return "", errors.New("registry access requires a stable space ID")
	}
	policyName := "borq-registry-" + spaceID
	policy := fmt.Sprintf(`path %q { capabilities = ["read"] }
path "auth/token/renew-self" { capabilities = ["update"] }
path "auth/token/lookup-self" { capabilities = ["read"] }
`, "secret/data/borq/registry-auth/"+spaceID+"/*")
	if err := vault.PutPolicy(ctx, policyName, policy); err != nil {
		return "", err
	}
	ref := "borq/registry-access/" + spaceID
	stored, err := vault.ReadKv(ctx, "secret", ref, 0)
	if err != nil && !errors.Is(err, openbao.ErrNotFound) {
		return "", err
	}
	if token := stored.Values["token"]; token != "" && vault.RenewToken(ctx, token) == nil {
		return token, nil
	}
	token, err := vault.CreatePeriodicToken(ctx, policyName)
	if err != nil {
		return "", err
	}
	if _, err := vault.WriteKv(ctx, "secret", ref, openbao.WriteKvInput{Values: map[string]string{"token": token}, CheckVersion: stored.Version}); err != nil {
		if !errors.Is(err, openbao.ErrConflict) {
			return "", err
		}
		winner, readErr := vault.ReadKv(ctx, "secret", ref, 0)
		if readErr != nil || winner.Values["token"] == "" {
			return "", errors.New("registry access token is unavailable")
		}
		return winner.Values["token"], nil
	}
	return token, nil
}

func (manager Manager) renewSpaceRegistries(ctx context.Context, cluster Cluster) error {
	spaces, err := manager.ListSpaces(ctx, cluster.Organization)
	if err != nil {
		return err
	}
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: cluster.KubeconfigPath, Context: cluster.Context, InCluster: cluster.InCluster}, logger.Logger{})
	if err != nil {
		return err
	}
	for _, space := range spaces {
		if space.Cluster != cluster.Name {
			continue
		}
		registries, err := manager.readRegistries(ctx, space.ID)
		if err != nil {
			return err
		}
		if len(registries) == 0 {
			continue
		}
		if _, err := connection.Namespaces().Get(ctx, space.Namespace); apierrors.IsNotFound(err) {
			continue
		} else if err != nil {
			return err
		}
		if err := manager.prepareSpaceRegistries(ctx, cluster, space, registries); err != nil {
			return err
		}
	}
	return nil
}

// Kubernetes object APIs require a pointer to track resource metadata.
func registryStore(ctx context.Context, connection kubernetes.Connection, namespace string, management bool, managementNamespace string) (*unstructured.Unstructured, error) {
	if managementNamespace == "" {
		managementNamespace = platform.ManagementNamespace
	}
	controller, sourceName := platform.SystemNamespace, "borq-openbao"
	kinds := []string{"ClusterSecretStore"}
	if management {
		controller, sourceName, kinds = platform.ManagementNamespace, "borq-management-openbao", []string{"SecretStore", "ClusterSecretStore"}
	}
	provider := map[string]any{"server": "http://openbao." + managementNamespace + ".svc:8200"}
	for _, kind := range kinds {
		source := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "external-secrets.io/v1", "kind": kind, "metadata": map[string]any{"name": sourceName}}}
		if kind == "SecretStore" {
			source.SetNamespace(namespace)
		}
		live, err := connection.Objects().Get(ctx, source)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		object := live.(*unstructured.Unstructured).Object
		// Preserve the installed controller class and vault transport, including legacy unclassified stores.
		controller, _, _ = unstructured.NestedString(object, "spec", "controller")
		for _, key := range []string{"server", "caBundle"} {
			value, found, err := unstructured.NestedString(object, "spec", "provider", "vault", key)
			if err != nil {
				return nil, err
			}
			if found {
				provider[key] = value
			}
		}
		break
	}
	provider["path"], provider["version"] = "secret", "v2"
	credential := "borq-registry-openbao"
	if management {
		credential = "borq-openbao-admin"
	}
	provider["auth"] = map[string]any{"tokenSecretRef": map[string]any{"name": credential, "key": "token"}}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "external-secrets.io/v1", "kind": "SecretStore",
		"metadata": map[string]any{"name": "borq-registry-openbao", "namespace": namespace, "labels": map[string]any{"app.kubernetes.io/managed-by": "borq"}},
		"spec":     map[string]any{"controller": controller, "provider": map[string]any{"vault": provider}},
	}}, nil
}

func (manager Manager) activateManagementRegistries(ctx context.Context, registries map[string]RegistryAuth) error {
	if len(registries) == 0 {
		return nil
	}
	namespace := manager.runtime.platformSettings.Namespace
	if namespace == "" {
		namespace = platform.ManagementNamespace
	}
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: manager.runtime.managementKubeconfigPath}, logger.Logger{})
	if err != nil {
		return err
	}
	store, err := registryStore(ctx, connection, namespace, true, namespace)
	if err != nil {
		return err
	}
	if err := connection.Objects().Apply(ctx, store); err != nil {
		return err
	}
	client, err := newClusterClient(Cluster{KubeconfigPath: manager.runtime.managementKubeconfigPath, InCluster: manager.runtime.managementKubeconfigPath == ""})
	if err != nil {
		return err
	}
	manifests := registryExternalSecrets(registries, namespace, "management", "borq-registry-openbao")
	if err := client.Apply(ctx, manifests, "management"); err != nil {
		return err
	}
	configuration := connection.RestConfig()
	// The Kubernetes SDK requires a pointer to the transport configuration.
	kubernetesClient, err := kubeclient.NewForConfig(&configuration)
	if err != nil {
		return err
	}
	deployment, err := kubernetesClient.AppsV1().Deployments(namespace).Get(ctx, "borq", metav1.GetOptions{})
	if err != nil {
		return err
	}
	pullSecrets := []corev1.LocalObjectReference{}
	for _, secret := range deployment.Spec.Template.Spec.ImagePullSecrets {
		if !strings.HasPrefix(secret.Name, "borq-registry-") {
			pullSecrets = append(pullSecrets, secret)
		}
	}
	for _, host := range slices.Sorted(maps.Keys(registries)) {
		pullSecrets = append(pullSecrets, corev1.LocalObjectReference{Name: registrySecretName(host, registries[host])})
	}
	patch, err := json.Marshal(map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"imagePullSecrets": pullSecrets}}}})
	if err != nil {
		return err
	}
	if _, err := kubernetesClient.AppsV1().Deployments(namespace).Patch(ctx, "borq", types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return err
	}
	return manager.saveRegistries(ctx, "management", registries)
}

func splitRegistryManifests(manifests []map[string]any) ([]map[string]any, []map[string]any) {
	registries, remaining := []map[string]any{}, []map[string]any{}
	for _, manifest := range manifests {
		labels, _, _ := unstructured.NestedStringMap(manifest, "metadata", "labels")
		if manifest["kind"] == "ExternalSecret" && labels["borq.run/phase"] == "registry" {
			registries = append(registries, manifest)
		} else {
			remaining = append(remaining, manifest)
		}
	}
	return registries, remaining
}

// Validate every projected generation, including credentials pinned by excluded apps.
func (manager Manager) verifyRegistryManifests(ctx context.Context, scope string, manifests []map[string]any) error {
	for _, manifest := range manifests {
		entries, _, err := unstructured.NestedSlice(manifest, "spec", "dataFrom")
		if err != nil {
			return err
		}
		for _, entry := range entries {
			extract, ok := entry.(map[string]any)
			if !ok {
				return fmt.Errorf("%w: invalid registry projection", ErrInvalid)
			}
			ref := manifestText(extract, "extract", "key")
			if _, err := manager.stageRegistries(ctx, map[string]RegistryAuth{registryManifestSecretName(manifest): {CredentialsRef: ref}}, scope); err != nil {
				return fmt.Errorf("%w: registry credentials were deleted or are unavailable; create a new plan", ErrConflict)
			}
		}
	}
	return nil
}

func (manager Manager) applyRegistryManifests(ctx context.Context, client kubeapps.Client, saved planRecord, manifests []map[string]any) error {
	manager.runtime.secretMutex.Lock()
	defer manager.runtime.secretMutex.Unlock()
	if err := manager.verifyRegistryManifests(ctx, saved.Plan.Space.ID, manifests); err != nil {
		return err
	}
	if err := manager.prepareSpaceRegistries(ctx, saved.Connection, saved.Plan.Space, saved.Registries); err != nil {
		return err
	}
	return client.Apply(ctx, manifests, saved.Plan.Space.ID)
}

func (manager Manager) omitDeletedRegistryProjections(ctx context.Context, scope string, manifests []map[string]any) ([]map[string]any, error) {
	registry, _ := splitRegistryManifests(manifests)
	if len(registry) == 0 {
		return manifests, nil
	}
	vault, err := platform.ManagementVault(ctx, manager.runtime.managementKubeconfigPath, manager.runtime.platformSettings.Namespace)
	if err != nil {
		return nil, err
	}
	deleted := map[string]bool{}
	for _, manifest := range registry {
		entries, _, err := unstructured.NestedSlice(manifest, "spec", "dataFrom")
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			object, ok := entry.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%w: invalid registry projection", ErrInvalid)
			}
			ref := manifestText(object, "extract", "key")
			if err := validateRegistryReference(ref, scope); err != nil {
				return nil, err
			}
			_, err = vault.ReadKv(ctx, "secret", ref, 1)
			if errors.Is(err, openbao.ErrNotFound) {
				deleted[manifestIdentity(manifest)] = true
				continue
			}
			if err != nil {
				return nil, err
			}
		}
	}
	result := []map[string]any{}
	for _, manifest := range manifests {
		if !deleted[manifestIdentity(manifest)] {
			result = append(result, manifest)
		}
	}
	return result, nil
}

func registryManifestSecretName(manifest map[string]any) string {
	if name := manifestText(manifest, "metadata", "annotations", "borq.run/registry-secret"); name != "" {
		return name
	}
	return "ghcr.io"
}
