package platform

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"time"

	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	"github.com/stefanmcshane/borq/foundations/openbao"
	"k8s.io/client-go/rest"
)

// CredentialsInput identifies the management connection and the registered target.
type CredentialsInput struct {
	// ManagementKubeconfigPath is empty for an in-cluster Borq server.
	ManagementKubeconfigPath string
	// Namespace contains the management vault and its admin Secret.
	Namespace string
	// TargetID scopes both the KV mount and stored credentials.
	TargetID string
}

// ClusterCredentials contains private credentials passed only to target installation.
type ClusterCredentials struct {
	// Mount isolates the target's versioned documents from every other cluster.
	Mount string `json:"-"`
	// VaultToken accesses only this cluster's mount and renews while Borq is running.
	VaultToken string `json:"-"`
	// TelemetryToken authenticates writes through Borq's ingestion gateway.
	TelemetryToken string `json:"-"`
}

// ClusterID gives a registration a stable, namespace-independent identity.
func ClusterID(organization, name string) string {
	digest := sha256.Sum256([]byte(organization + "/" + name))
	return hex.EncodeToString(digest[:16])
}

// ManagementVault reads the bootstrap admin credential and uses the management
// API Service proxy. The SDK requires a pointer to own its shared HTTP transport.
func ManagementVault(ctx context.Context, kubeconfigPath, namespace string) (*openbao.Client, error) {
	if namespace == "" {
		namespace = ManagementNamespace
	}
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: kubeconfigPath}, logger.Logger{})
	if err != nil {
		return nil, err
	}
	vault, err := OpenManagementVault(ctx, connection, namespace)
	if err != nil {
		return nil, err
	}
	status, err := vault.Status(ctx)
	if err != nil {
		return nil, errors.New("management vault is unavailable")
	}
	if !status.Initialized {
		return nil, errors.New("management vault has not been initialized")
	}
	if status.Sealed {
		if err := vault.WaitForActive(ctx, time.Second, 30*time.Second); err != nil {
			return nil, err
		}
	}
	return vault, nil
}

// OpenManagementVault opens the admin connection using an already selected cluster.
// The SDK requires shared pointer ownership of its HTTP transport.
func OpenManagementVault(ctx context.Context, connection kubernetes.Connection, namespace string) (*openbao.Client, error) {
	if namespace == "" {
		namespace = ManagementNamespace
	}
	secret, err := connection.Secrets().Get(ctx, namespace, "borq-openbao-admin")
	if err != nil {
		return nil, fmt.Errorf("read management vault admin credential: %w", err)
	}
	token := secret.Data["token"]
	if len(token) == 0 {
		return nil, fmt.Errorf("management vault admin credential is empty")
	}
	configuration := connection.RestConfig()
	client, err := rest.HTTPClientFor(&configuration)
	if err != nil {
		return nil, fmt.Errorf("configure management vault transport: %w", err)
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return fmt.Errorf("management vault redirects are disabled")
	}
	return openbao.NewClient(openbao.NewClientInput{
		Address: strings.TrimRight(configuration.Host, "/") + "/api/v1/namespaces/" + namespace + "/services/http:openbao:8200/proxy",
		Token:   string(token), HttpClient: *client,
	})
}

// EnsureClusterCredentials creates or renews one scoped vault token and independent
// stable telemetry and DNS challenge keys. Callers serialize reconciliation so concurrent registration cannot issue competing keys.
func EnsureClusterCredentials(ctx context.Context, input CredentialsInput) (ClusterCredentials, error) {
	if len(input.TargetID) != 32 || strings.Trim(input.TargetID, "0123456789abcdef") != "" {
		return ClusterCredentials{}, fmt.Errorf("a stable cluster ID is required")
	}
	vault, err := ManagementVault(ctx, input.ManagementKubeconfigPath, input.Namespace)
	if err != nil {
		return ClusterCredentials{}, err
	}
	mount := "borq-" + input.TargetID
	if err := vault.EnsureKvV2(ctx, mount); err != nil {
		return ClusterCredentials{}, err
	}
	policy := fmt.Sprintf(`path %q { capabilities = ["create", "read", "update", "list"] }
path %q { capabilities = ["read"] }
path "auth/token/renew-self" { capabilities = ["update"] }
path "auth/token/lookup-self" { capabilities = ["read"] }
`, mount+"/*", "database-"+input.TargetID+"/static-creds/*")
	if err := vault.PutPolicy(ctx, mount, policy); err != nil {
		return ClusterCredentials{}, err
	}
	path := "borq/clusters/" + input.TargetID + "/credentials"
	stored, err := vault.ReadKv(ctx, "secret", path, 0)
	if err != nil && !errors.Is(err, openbao.ErrNotFound) {
		return ClusterCredentials{}, err
	}
	values := maps.Clone(stored.Values)
	if values == nil {
		values = map[string]string{}
	}
	changed := false
	if values["openbao_token"] == "" || vault.RenewToken(ctx, values["openbao_token"]) != nil {
		token, err := vault.CreatePeriodicToken(ctx, mount)
		if err != nil {
			return ClusterCredentials{}, err
		}
		values["openbao_token"] = token
		changed = true
	}
	for _, tokenName := range []string{"telemetry_token"} {
		if values[tokenName] == "" {
			key := make([]byte, 32)
			if _, err := rand.Read(key); err != nil {
				return ClusterCredentials{}, fmt.Errorf("generate cluster authentication key: %w", err)
			}
			values[tokenName] = hex.EncodeToString(key)
			changed = true
		}
	}
	if changed {
		if _, err := vault.WriteKv(ctx, "secret", path, openbao.WriteKvInput{Values: values, CheckVersion: stored.Version}); err != nil {
			return ClusterCredentials{}, err
		}
	}
	telemetry, err := vault.ReadKv(ctx, mount, "borq/platform/telemetry", 0)
	if err != nil && !errors.Is(err, openbao.ErrNotFound) {
		return ClusterCredentials{}, err
	}
	if telemetry.Values["TELEMETRY_TOKEN"] != values["telemetry_token"] {
		if _, err := vault.WriteKv(ctx, mount, "borq/platform/telemetry", openbao.WriteKvInput{Values: map[string]string{"TELEMETRY_TOKEN": values["telemetry_token"]}, CheckVersion: telemetry.Version}); err != nil {
			return ClusterCredentials{}, err
		}
	}

	return ClusterCredentials{Mount: mount, VaultToken: values["openbao_token"], TelemetryToken: values["telemetry_token"]}, nil
}

// ReadTelemetryToken retrieves the current key without minting tokens or writing vault versions.
func ReadTelemetryToken(ctx context.Context, input CredentialsInput) (string, error) {
	vault, err := ManagementVault(ctx, input.ManagementKubeconfigPath, input.Namespace)
	if err != nil {
		return "", err
	}
	entry, err := vault.ReadKv(ctx, "secret", "borq/clusters/"+input.TargetID+"/credentials", 0)
	if err != nil {
		return "", err
	}
	if entry.Values["telemetry_token"] == "" {
		return "", fmt.Errorf("cluster telemetry key is unavailable")
	}
	return entry.Values["telemetry_token"], nil
}
