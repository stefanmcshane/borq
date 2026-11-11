package apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/sqlite"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type managementNetworkConfiguration struct {
	BorqPublicURL string                   `json:"borqPublicUrl,omitempty"`
	Network       platform.NetworkSettings `json:"network"`
}

func (manager Manager) safeManagementNetwork(ctx context.Context) (*platform.NetworkSettings, error) {
	settings, err := manager.GetPlatformSettings(ctx)
	if err != nil {
		return nil, err
	}
	if settings.Network.Cloudflare == nil && settings.Network.Tailscale == nil {
		return nil, nil
	}
	return &settings.Network, nil
}

func (manager Manager) applyManagementNetworkConfiguration(ctx context.Context, configuration ManagementConfiguration) error {
	manager.runtime.platformMutex.Lock()
	defer manager.runtime.platformMutex.Unlock()
	settings, err := manager.GetPlatformSettings(ctx)
	if err != nil {
		return err
	}
	// Existing installations already chose their address under the legacy naming
	// rule. Retain that address; fresh installations have no configured provider.
	if settings.BorqPublicURL == "" && settings.Network.Cloudflare != nil {
		if host := platform.ManagementPublicHostname(settings); host != "" {
			settings.BorqPublicURL = "https://" + host
		}
	}
	if configuration.BorqPublicURL != "" {
		if settings.BorqPublicURL != "" && settings.BorqPublicURL != strings.TrimRight(strings.TrimSpace(configuration.BorqPublicURL), "/") {
			providers, err := manager.database.List(ctx, "organization-oidc")
			if err != nil {
				return err
			}
			for _, record := range providers {
				var provider organizationOidcRecord
				if json.Unmarshal(record.Value, &provider) != nil {
					return errors.New("could not read existing organization identity settings")
				}
				if provider.Settings.BaseURL == settings.BorqPublicURL {
					return fmt.Errorf("%w: changing borqPublicUrl requires migrating existing organization callbacks before changing public routing", ErrConflict)
				}
			}
			policies, err := manager.database.List(ctx, "organization-login-policy")
			if err != nil {
				return err
			}
			for _, record := range policies {
				var policy organizationLoginPolicy
				if json.Unmarshal(record.Value, &policy) != nil {
					return errors.New("could not read retained organization administrator policy")
				}
				if policy.BaseURL == settings.BorqPublicURL {
					return fmt.Errorf("%w: changing borqPublicUrl requires migrating retained organization authentication before changing public routing", ErrConflict)
				}
			}
			pockets, err := manager.database.List(ctx, "organization-pocketid")
			if err != nil {
				return err
			}
			for _, record := range pockets {
				pocket, _, err := manager.readPocketId(ctx, record.Key)
				if err != nil {
					return err
				}
				if pocket.Status.BaseURL == settings.BorqPublicURL {
					return fmt.Errorf("%w: changing borqPublicUrl requires migrating retained Pocket ID callbacks before changing public routing", ErrConflict)
				}
			}
		}
		settings.BorqPublicURL = strings.TrimRight(strings.TrimSpace(configuration.BorqPublicURL), "/")
	}
	if configuration.Network != nil {
		if configuration.Network.Tailscale != nil {
			settings.Network.Tailscale = configuration.Network.Tailscale
		}
		if configuration.Network.Cloudflare != nil {
			settings.Network.Cloudflare = configuration.Network.Cloudflare
		}
	}
	settings.Network.ManagementURL = ""
	settings.Network, err = platform.NormalizeNetworkSettings(settings.Network, settings.ClusterName)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if settings.Network.Cloudflare != nil {
		address, err := url.Parse(settings.BorqPublicURL)
		if err != nil || address.Scheme != "https" || address.User != nil || address.Port() != "" || address.Path != "" || address.RawQuery != "" || address.Fragment != "" ||
			!networkDnsManagementHost(address.Hostname(), settings.Network.Cloudflare.WildcardDomain) {
			return fmt.Errorf("%w: specify borqPublicUrl as an HTTPS origin within the configured Cloudflare domain", ErrInvalid)
		}
	}
	client, namespace, err := manager.managementInputClient()
	if err != nil {
		return err
	}
	configMap, configErr := client.CoreV1().ConfigMaps(namespace).Get(ctx, "borq-network-settings", metav1.GetOptions{})
	if configErr != nil && !apierrors.IsNotFound(configErr) {
		return errors.New("could not read mounted management network settings")
	}
	if configErr == nil && (configMap.Labels["app.kubernetes.io/managed-by"] != "borq" || len(configMap.OwnerReferences) != 0 || configMap.DeletionTimestamp != nil) {
		return fmt.Errorf("%w: mounted management network settings are owned outside Borq", ErrConflict)
	}
	const name = "borq-network-credentials"
	secret, err := client.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	create := apierrors.IsNotFound(err)
	if err != nil && !create {
		return errors.New("could not read management network credentials")
	}
	if create {
		secret = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: map[string]string{"app.kubernetes.io/managed-by": "borq", "app.kubernetes.io/part-of": "borq", "app.kubernetes.io/name": name}}, Data: map[string][]byte{}}
	} else if secret.Labels["app.kubernetes.io/managed-by"] != "borq" || secret.Labels["app.kubernetes.io/part-of"] != "borq" || secret.Labels["app.kubernetes.io/name"] != name {
		return fmt.Errorf("%w: management network credentials are owned outside Borq", ErrConflict)
	}
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	if provider := settings.Network.Tailscale; provider != nil {
		if provider.CredentialsRef != "" && (provider.ClientID != "" || provider.ClientSecret != "") {
			return fmt.Errorf("%w: supply Tailscale client credentials or credentialsRef, not both", ErrInvalid)
		}
		if provider.CredentialsRef != "" && provider.CredentialsRef != name {
			return fmt.Errorf("%w: Tailscale credentialsRef must reference this installation's management network credentials", ErrInvalid)
		}
		if (provider.ClientID == "") != (provider.ClientSecret == "") {
			return fmt.Errorf("%w: specify both Tailscale clientId and clientSecret", ErrInvalid)
		}
		if provider.ClientID != "" {
			secret.Data["tailscaleClientId"], secret.Data["tailscaleClientSecret"] = []byte(provider.ClientID), []byte(provider.ClientSecret)
		}
		if len(secret.Data["tailscaleClientId"]) == 0 || len(secret.Data["tailscaleClientSecret"]) == 0 {
			return fmt.Errorf("%w: Tailscale requires explicit clientId and clientSecret on its first apply", ErrInvalid)
		}
		provider.ClientID, provider.ClientSecret, provider.CredentialsRef = "", "", name
	}
	if provider := settings.Network.Cloudflare; provider != nil {
		if provider.CredentialsRef != "" && provider.ApiToken != "" {
			return fmt.Errorf("%w: supply Cloudflare apiToken or credentialsRef, not both", ErrInvalid)
		}
		if provider.CredentialsRef != "" && provider.CredentialsRef != name {
			return fmt.Errorf("%w: Cloudflare credentialsRef must reference this installation's management network credentials", ErrInvalid)
		}
		if provider.ApiToken != "" {
			secret.Data["cloudflareApiToken"] = []byte(provider.ApiToken)
		}
		if len(secret.Data["cloudflareApiToken"]) == 0 {
			return fmt.Errorf("%w: Cloudflare requires an explicit apiToken on its first apply", ErrInvalid)
		}
		provider.ApiToken, provider.CredentialsRef = "", name
	}
	if settings.Network.Tailscale != nil || settings.Network.Cloudflare != nil {
		if create {
			_, err = client.CoreV1().Secrets(namespace).Create(ctx, secret, metav1.CreateOptions{})
		} else {
			_, err = client.CoreV1().Secrets(namespace).Update(ctx, secret, metav1.UpdateOptions{})
		}
		if err != nil {
			return errors.New("could not store management network credentials")
		}
	}
	stored := managementNetworkConfiguration{BorqPublicURL: settings.BorqPublicURL, Network: settings.Network}
	_, previous, err := loadDocument[managementNetworkConfiguration](ctx, manager.database, "management-network-settings", "shared")
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	write, err := documentWrite("management-network-settings", "shared", stored, previous.Revision)
	if err != nil {
		return err
	}
	// Teardown reads this receipt independently of SQLite, including after a
	// minimal bootstrap which has never created mounted network configuration.
	encoded, err := json.Marshal(settings.Network)
	if err != nil {
		return err
	}
	createSettings := apierrors.IsNotFound(configErr)
	if createSettings {
		configMap = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "borq-network-settings", Namespace: namespace,
			Labels: map[string]string{"app.kubernetes.io/managed-by": "borq", "app.kubernetes.io/part-of": "borq", "app.kubernetes.io/name": "borq-network-settings", "app.kubernetes.io/component": "network"}}}
	}
	if configMap.Data == nil {
		configMap.Data = map[string]string{}
	}
	configMap.Data["network.json"], configMap.Data["clusterName"] = string(encoded), settings.ClusterName
	if createSettings {
		_, err = client.CoreV1().ConfigMaps(namespace).Create(ctx, configMap, metav1.CreateOptions{})
	} else {
		_, err = client.CoreV1().ConfigMaps(namespace).Update(ctx, configMap, metav1.UpdateOptions{})
	}
	if err != nil {
		return err
	}
	return manager.database.Commit(ctx, []sqlite.Write{write})
}
