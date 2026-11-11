package apps

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/sqlite"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
)

// TestManagementNetworkConfiguration retains explicit credentials only in the
// owned Kubernetes Secret, and restores public settings through a real database reopen.
// The HTTP fixture implements storage requests, not networking controllers.
func TestManagementNetworkConfiguration(t *testing.T) {
	assert := is.New(t)
	fixture := newManagementInputFixture(t)
	manager, err := NewManager(t.Context(), fixture.input)
	assert.NoErr(err)
	defer func() { _ = manager.Close() }()
	configuration := managementNetworkDeclaration()
	assert.NoErr(manager.applyManagementNetworkConfiguration(t.Context(), configuration))
	client, namespace, err := manager.managementInputClient()
	assert.NoErr(err)
	secret, err := client.CoreV1().Secrets(namespace).Get(t.Context(), "borq-network-credentials", metav1.GetOptions{})
	assert.NoErr(err)
	assert.Equal(secret.Labels["app.kubernetes.io/managed-by"], "borq")
	assert.Equal(string(secret.Data["tailscaleClientId"]), "private-tailscale-client-id")
	assert.Equal(string(secret.Data["tailscaleClientSecret"]), "private-tailscale-client-secret")
	assert.Equal(string(secret.Data["cloudflareApiToken"]), "private-cloudflare-api-token")
	// Normalization must not erase the caller's input while staging it.
	assert.Equal(configuration.Network.Tailscale.ClientSecret, "private-tailscale-client-secret")
	assert.Equal(configuration.Network.Cloudflare.ApiToken, "private-cloudflare-api-token")
	assert.NoErr(manager.Close())
	assertManagementInputPrivate(t, fixture.input.DatabasePath)
	manager, err = NewManager(t.Context(), fixture.input)
	assert.NoErr(err)
	settings, err := manager.GetPlatformSettings(t.Context())
	assert.NoErr(err)
	assert.Equal(settings.ClusterName, "default")
	assert.Equal(settings.BorqPublicURL, "https://control.example.test")
	assert.Equal(platform.ManagementPublicHostname(settings), "control.example.test")
	assert.Equal(settings.Network.Tailscale.CredentialsRef, "borq-network-credentials")
	assert.Equal(settings.Network.Cloudflare.CredentialsRef, "borq-network-credentials")
	assert.Equal(settings.Network.Tailscale.ClientID, "")
	assert.Equal(settings.Network.Tailscale.ClientSecret, "")
	assert.Equal(settings.Network.Cloudflare.ApiToken, "")
	assert.Equal(settings.Network.ManagementURL, "") // No controller-assigned address has been observed.
	normalized, err := manager.normalizeManagementIdentity(t.Context(), ManagementConfiguration{Organizations: []ManagementOrganization{{Name: "new-team", ManagedPocketId: &ManagementPocketId{IssuerURL: "https://id-new-team.example.test", AdminEmail: "owner@example.test"}}}})
	assert.NoErr(err)
	assert.Equal(normalized.Organizations[0].ManagedPocketId.BaseURL, settings.BorqPublicURL)
	exported, err := manager.GetManagementConfiguration(t.Context())
	assert.NoErr(err)
	encoded, err := json.Marshal(exported)
	assert.NoErr(err)
	assertManagementInputRedacted(t, encoded)
	_, _, err = loadDocument[managementNetworkConfiguration](t.Context(), manager.database, "management-network-settings", "shared")
	assert.NoErr(err)
	assert.NoErr(manager.applyManagementNetworkConfiguration(t.Context(), exported))
	retained, err := client.CoreV1().Secrets(namespace).Get(t.Context(), secret.Name, metav1.GetOptions{})
	assert.NoErr(err)
	assert.Equal(retained.UID, secret.UID)
	assert.Equal(retained.Data, secret.Data)
}

// TestManagementNetworkInputGuards reaches each credential and ownership guard
// before configuration is saved; the fixture never supplies hidden credentials.
func TestManagementNetworkInputGuards(t *testing.T) {
	for _, test := range []struct {
		name    string
		change  func(*ManagementConfiguration)
		foreign bool
		message string
	}{
		{"missing tailscale credentials", func(configuration *ManagementConfiguration) {
			configuration.Network.Tailscale.ClientID, configuration.Network.Tailscale.ClientSecret = "", ""
		}, false, "Tailscale requires explicit"},
		{"partial tailscale credentials", func(configuration *ManagementConfiguration) { configuration.Network.Tailscale.ClientSecret = "" }, false, "specify both Tailscale"},
		{"tailscale foreign reference", func(configuration *ManagementConfiguration) {
			configuration.Network.Tailscale.ClientID, configuration.Network.Tailscale.ClientSecret = "", ""
			configuration.Network.Tailscale.CredentialsRef = "foreign-secret"
		}, false, "Tailscale credentialsRef"},
		{"missing cloudflare credentials", func(configuration *ManagementConfiguration) { configuration.Network.Cloudflare.ApiToken = "" }, false, "Cloudflare requires an explicit"},
		{"cloudflare foreign reference", func(configuration *ManagementConfiguration) {
			configuration.Network.Cloudflare.ApiToken = ""
			configuration.Network.Cloudflare.CredentialsRef = "foreign-secret"
		}, false, "Cloudflare credentialsRef"},
		{"tailscale conflicting credentials", func(configuration *ManagementConfiguration) {
			configuration.Network.Tailscale.CredentialsRef = "borq-network-credentials"
		}, false, "not both"},
		{"cloudflare conflicting credentials", func(configuration *ManagementConfiguration) {
			configuration.Network.Cloudflare.CredentialsRef = "borq-network-credentials"
		}, false, "not both"},
		{"foreign secret", func(*ManagementConfiguration) {}, true, "owned outside Borq"},
		{"public address outside domain", func(configuration *ManagementConfiguration) {
			configuration.BorqPublicURL = "https://control.other.test"
		}, false, "specify borqPublicUrl"},
		{"missing public address", func(configuration *ManagementConfiguration) { configuration.BorqPublicURL = "" }, false, "specify borqPublicUrl"},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			fixture := newManagementInputFixture(t)
			manager, err := NewManager(t.Context(), fixture.input)
			assert.NoErr(err)
			defer func() { _ = manager.Close() }()
			if test.foreign {
				client, namespace, err := manager.managementInputClient()
				assert.NoErr(err)
				_, err = client.CoreV1().Secrets(namespace).Create(t.Context(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "borq-network-credentials", Namespace: namespace, Labels: map[string]string{"app.kubernetes.io/managed-by": "other"}}, Data: map[string][]byte{"untouched": []byte("retained")}}, metav1.CreateOptions{})
				assert.NoErr(err)
			}
			configuration := managementNetworkDeclaration()
			test.change(&configuration)
			err = manager.applyManagementNetworkConfiguration(t.Context(), configuration)
			assert.True(errors.Is(err, ErrInvalid) || errors.Is(err, ErrConflict))
			assert.True(strings.Contains(err.Error(), test.message))
			_, _, err = loadDocument[managementNetworkConfiguration](t.Context(), manager.database, "management-network-settings", "shared")
			assert.True(errors.Is(err, ErrNotFound))
			fixture.mutex.Lock()
			updates := fixture.updates
			fixture.mutex.Unlock()
			assert.Equal(updates, 0)
		})
	}
}

func managementNetworkDeclaration() ManagementConfiguration {
	return ManagementConfiguration{BorqPublicURL: "https://control.example.test", Network: &platform.NetworkSettings{
		Tailscale:  &platform.TailscaleSettings{ClientID: "private-tailscale-client-id", ClientSecret: "private-tailscale-client-secret"},
		Cloudflare: &platform.CloudflareSettings{Exposure: "tunnel", WildcardDomain: "*.example.test", ZoneID: strings.Repeat("a", 32), AcmeEmail: "admin@example.test", ApiToken: "private-cloudflare-api-token"},
	}}
}

type managementInputFixture struct {
	input               NewInput
	mutex               sync.Mutex
	secrets             map[string]*corev1.Secret
	updates             int
	created             int
	configMap           *corev1.ConfigMap
	denyConfigMapWrites bool
	configMapWrites     int
	gate                chan struct{}
	started             chan struct{}
}

func newManagementInputFixture(t *testing.T) *managementInputFixture {
	t.Helper()
	fixture := &managementInputFixture{secrets: map[string]*corev1.Secret{}, started: make(chan struct{}, 1)}
	endpoint := httptest.NewServer(http.HandlerFunc(fixture.serve))
	t.Cleanup(endpoint.Close)
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	configuration := fmt.Sprintf("apiVersion: v1\nkind: Config\nclusters:\n- name: default\n  cluster:\n    server: %s\ncontexts:\n- name: default\n  context:\n    cluster: default\n    user: default\ncurrent-context: default\nusers:\n- name: default\n  user: {}\n", endpoint.URL)
	if err := os.WriteFile(kubeconfig, []byte(configuration), 0600); err != nil {
		t.Fatal(err)
	}
	fixture.input = NewInput{DatabasePath: filepath.Join(t.TempDir(), "state.sqlite"), ManagementKubeconfigPath: kubeconfig,
		PollInterval: time.Millisecond, PlatformSettings: platform.Settings{Namespace: "borq", ClusterName: "default"}}
	return fixture
}

func (fixture *managementInputFixture) serve(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Content-Type", "application/json")
	const prefix = "/api/v1/namespaces/borq/secrets"
	name := strings.TrimPrefix(request.URL.Path, prefix+"/")
	if request.Method == http.MethodGet && strings.HasPrefix(name, "borq-management-input-") {
		fixture.mutex.Lock()
		gate := fixture.gate
		fixture.mutex.Unlock()
		if gate != nil {
			select {
			case fixture.started <- struct{}{}:
			default:
			}
			select {
			case <-gate:
			case <-request.Context().Done():
				return
			}
		}
	}
	fixture.mutex.Lock()
	defer fixture.mutex.Unlock()
	if strings.HasPrefix(request.URL.Path, "/api/v1/namespaces/borq/configmaps") && (request.Method == http.MethodPost || request.Method == http.MethodPut) {
		fixture.configMapWrites++
		if fixture.denyConfigMapWrites {
			writer.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(writer).Encode(metav1.Status{Status: "Failure", Reason: metav1.StatusReasonForbidden, Code: http.StatusForbidden})
			return
		}
	}
	if request.Method == http.MethodGet && (request.URL.Path == "/api/v1/namespaces/borq" || request.URL.Path == "/api/v1/namespaces/kube-system") {
		_ = json.NewEncoder(writer).Encode(corev1.Namespace{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"}, ObjectMeta: metav1.ObjectMeta{Name: filepath.Base(request.URL.Path), UID: types.UID("fixture-" + filepath.Base(request.URL.Path))}})
		return
	}
	if request.URL.Path == "/api/v1/namespaces/borq/configmaps" && request.Method == http.MethodPost {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		var configMap corev1.ConfigMap
		if _, _, err := scheme.Codecs.UniversalDeserializer().Decode(body, nil, &configMap); err != nil {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		configMap.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}
		configMap.UID, configMap.ResourceVersion = "fixture-network-settings", "1"
		fixture.configMap = configMap.DeepCopy()
		writer.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(writer).Encode(configMap)
		return
	}
	if request.URL.Path == "/api/v1/namespaces/borq/configmaps/borq-network-settings" && fixture.configMap != nil {
		if request.Method == http.MethodGet {
			_ = json.NewEncoder(writer).Encode(fixture.configMap)
			return
		}
		if request.Method == http.MethodPut {
			body, err := io.ReadAll(request.Body)
			if err != nil {
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			var configMap corev1.ConfigMap
			if _, _, err := scheme.Codecs.UniversalDeserializer().Decode(body, nil, &configMap); err != nil {
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			fixture.configMap = configMap.DeepCopy()
			_ = json.NewEncoder(writer).Encode(configMap)
			return
		}
	}
	if request.URL.Path == prefix && request.Method == http.MethodPost {
		var secret corev1.Secret
		if err := decodeManagementInputSecret(request, &secret); err != nil {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		fixture.created++
		secret.UID, secret.ResourceVersion = types.UID(fmt.Sprintf("fixture-uid-%d", fixture.created)), "1"
		fixture.secrets[secret.Name] = secret.DeepCopy()
		writer.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(writer).Encode(secret)
		return
	}
	if strings.HasPrefix(request.URL.Path, prefix+"/") {
		secret, exists := fixture.secrets[name]
		if exists && request.Method == http.MethodGet {
			_ = json.NewEncoder(writer).Encode(secret)
			return
		}
		if exists && request.Method == http.MethodDelete {
			body, err := io.ReadAll(request.Body)
			if err != nil {
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			var options metav1.DeleteOptions
			if _, _, err := scheme.Codecs.UniversalDeserializer().Decode(body, nil, &options); err != nil {
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			if options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != secret.UID {
				writer.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(writer).Encode(metav1.Status{Status: "Failure", Reason: metav1.StatusReasonConflict, Code: http.StatusConflict})
				return
			}
			delete(fixture.secrets, name)
			_ = json.NewEncoder(writer).Encode(metav1.Status{Status: "Success"})
			return
		}
		if exists && request.Method == http.MethodPut {
			var update corev1.Secret
			if err := decodeManagementInputSecret(request, &update); err != nil {
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			fixture.updates++
			fixture.secrets[name] = update.DeepCopy()
			_ = json.NewEncoder(writer).Encode(update)
			return
		}
	}
	writer.WriteHeader(http.StatusNotFound)
	_ = json.NewEncoder(writer).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Failure", Reason: metav1.StatusReasonNotFound, Code: http.StatusNotFound})
}

func assertManagementInputPrivate(t *testing.T, databasePath string) {
	t.Helper()
	contents, err := os.ReadFile(databasePath)
	is.New(t).NoErr(err)
	assertManagementInputRedacted(t, contents)
}

func assertManagementInputRedacted(t *testing.T, contents []byte) {
	t.Helper()
	for _, secret := range []string{"private-tailscale-client-id", "private-tailscale-client-secret", "private-cloudflare-api-token", "private-oidc-client-secret"} {
		if strings.Contains(string(contents), secret) {
			t.Fatal("private management credential appeared outside Kubernetes Secret storage")
		}
	}
}

func decodeManagementInputSecret(request *http.Request, secret *corev1.Secret) error {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return err
	}
	_, _, err = scheme.Codecs.UniversalDeserializer().Decode(body, nil, secret)
	return err
}

// TestManagementNetworkMountedSettings refuses foreign mounted configuration
// before any activation and initializes an owned ConfigMap without assuming data exists.
func TestManagementNetworkMountedSettings(t *testing.T) {
	for _, owner := range []string{"borq", "other", "foreign owner reference", "deleting"} {
		t.Run(owner, func(t *testing.T) {
			assert := is.New(t)
			fixture := newManagementInputFixture(t)
			fixture.configMap = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "borq-network-settings", Namespace: "borq", Labels: map[string]string{"app.kubernetes.io/managed-by": owner}}}
			if owner == "foreign owner reference" {
				fixture.configMap.Labels["app.kubernetes.io/managed-by"] = "borq"
				fixture.configMap.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: "foreign", UID: "foreign-uid"}}
			}
			if owner == "deleting" {
				fixture.configMap.Labels["app.kubernetes.io/managed-by"] = "borq"
				now := metav1.Now()
				fixture.configMap.DeletionTimestamp = &now
			}
			manager, err := NewManager(t.Context(), fixture.input)
			assert.NoErr(err)
			defer func() { _ = manager.Close() }()
			err = manager.applyManagementNetworkConfiguration(t.Context(), managementNetworkDeclaration())
			if owner != "borq" {
				assert.True(errors.Is(err, ErrConflict))
				assert.True(strings.Contains(err.Error(), "owned outside Borq"))
				_, _, err = loadDocument[managementNetworkConfiguration](t.Context(), manager.database, "management-network-settings", "shared")
				assert.True(errors.Is(err, ErrNotFound))
				fixture.mutex.Lock()
				count := len(fixture.secrets)
				fixture.mutex.Unlock()
				assert.Equal(count, 0)
				return
			}
			assert.NoErr(err)
			fixture.mutex.Lock()
			value := fixture.configMap.Data["network.json"]
			fixture.mutex.Unlock()
			var network platform.NetworkSettings
			assert.NoErr(json.Unmarshal([]byte(value), &network))
			assert.Equal(network.Cloudflare.WildcardDomain, "example.test")
			assert.Equal(network.Cloudflare.CredentialsRef, "borq-network-credentials")
			assertManagementInputRedacted(t, []byte(value))
		})
	}
}

// TestManagementNetworkPendingIdentityAddress preserves the callback origin of
// an accepted Pocket ID even before it has generated a ready OIDC record.
func TestManagementNetworkPendingIdentityAddress(t *testing.T) {
	assert := is.New(t)
	fixture := newManagementInputFixture(t)
	manager, err := NewManager(t.Context(), fixture.input)
	assert.NoErr(err)
	defer func() { _ = manager.Close() }()
	assert.NoErr(manager.applyManagementNetworkConfiguration(t.Context(), managementNetworkDeclaration()))
	owner, err := manager.CreateOrganization(t.Context(), "team")
	assert.NoErr(err)
	pocket := organizationPocketIdRecord{ID: strings.Repeat("b", 32), Organization: owner.Name, OrganizationID: owner.ID,
		Status: OrganizationPocketId{State: "queued", IssuerURL: "https://id.example.test", BaseURL: "https://control.example.test", CertificateIssuer: "letsencrypt-cloudflare", AdminUsername: "owner@example.test", AdminEmail: "owner@example.test"}}
	write, err := documentWrite("organization-pocketid", owner.ID, pocket, 0)
	assert.NoErr(err)
	assert.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{write}))
	err = manager.applyManagementNetworkConfiguration(t.Context(), ManagementConfiguration{BorqPublicURL: "https://replacement.example.test"})
	assert.True(errors.Is(err, ErrConflict))
	assert.True(strings.Contains(err.Error(), "retained Pocket ID callbacks"))
	settings, err := manager.GetPlatformSettings(t.Context())
	assert.NoErr(err)
	assert.Equal(settings.BorqPublicURL, "https://control.example.test")
	fixture.mutex.Lock()
	updates := fixture.updates
	fixture.mutex.Unlock()
	assert.Equal(updates, 0)
}

// TestManagementNetworkReceiptForDestroy reads the ConfigMap created by first
// apply through the real API clients and includes its identity in the destroy plan.
// The fixture supplies storage metadata only; no controller or deletion is simulated.
func TestManagementNetworkReceiptForDestroy(t *testing.T) {
	assert := is.New(t)
	fixture := newManagementInputFixture(t)
	manager, err := NewManager(t.Context(), fixture.input)
	assert.NoErr(err)
	defer func() { _ = manager.Close() }()
	configuration := managementNetworkDeclaration()
	configuration.Network.Cloudflare = nil // Planning private networking requires no remote provider API.
	assert.NoErr(manager.applyManagementNetworkConfiguration(t.Context(), configuration))
	client, namespace, err := manager.managementInputClient()
	assert.NoErr(err)
	receipt, err := client.CoreV1().ConfigMaps(namespace).Get(t.Context(), "borq-network-settings", metav1.GetOptions{})
	assert.NoErr(err)
	assert.Equal(receipt.Labels["app.kubernetes.io/managed-by"], "borq")
	assert.Equal(receipt.Data["clusterName"], "default")
	assertManagementInputRedacted(t, []byte(receipt.Data["network.json"]))
	plan, err := platform.PlanManagementDestroy(t.Context(), platform.ManagementDestroyInput{KubeconfigPath: fixture.input.ManagementKubeconfigPath, Namespace: namespace})
	assert.NoErr(err)
	found := false
	for _, dependency := range plan.Dependencies {
		found = found || dependency.Name == receipt.Name && dependency.UID == string(receipt.UID)
	}
	assert.True(found)
}

// TestManagementNetworkReceiptFailureKeepsBootstrapGateClosed prevents a failed
// receipt write from activating component reconciliation without safe teardown metadata.
func TestManagementNetworkReceiptFailureKeepsBootstrapGateClosed(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprint(existing), func(t *testing.T) {
			assert := is.New(t)
			fixture := newManagementInputFixture(t)
			fixture.denyConfigMapWrites = true
			if existing {
				fixture.configMap = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "borq-network-settings", Namespace: "borq", Labels: map[string]string{"app.kubernetes.io/managed-by": "borq"}}}
			}
			manager, err := NewManager(t.Context(), fixture.input)
			assert.NoErr(err)
			defer func() { _ = manager.Close() }()
			err = manager.applyManagementNetworkConfiguration(t.Context(), managementNetworkDeclaration())
			assert.True(err != nil)
			fixture.mutex.Lock()
			attempts := fixture.configMapWrites
			fixture.mutex.Unlock()
			assert.Equal(attempts, 1)
			_, _, err = loadDocument[managementNetworkConfiguration](t.Context(), manager.database, "management-network-settings", "shared")
			assert.True(errors.Is(err, ErrNotFound))
			settings, err := manager.GetPlatformSettings(t.Context())
			assert.NoErr(err)
			assert.Equal(settings.BorqPublicURL, "")
			assert.True(settings.Network.Tailscale == nil && settings.Network.Cloudflare == nil)
		})
	}
}
