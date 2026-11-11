package apps

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"reflect"

	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/cloudflare"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// Optional provider blocks preserve current integrations when a partial file omits them.
func (manager Manager) validateBootstrapNetwork(input *platform.NetworkSettings) error {
	if input == nil {
		return nil
	}
	if manager.runtime.managementBootstrapOnly {
		_, err := platform.NormalizeNetworkSettings(*input, manager.runtime.platformSettings.ClusterName)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		return nil
	}
	wanted := *input
	current := manager.runtime.platformSettings.Network
	if wanted.Tailscale == nil {
		wanted.Tailscale = current.Tailscale
	}
	if wanted.Cloudflare == nil {
		wanted.Cloudflare = current.Cloudflare
	} else if wanted.Cloudflare.WebhookImage == "" && current.Cloudflare != nil {
		cloudflare := *wanted.Cloudflare
		cloudflare.WebhookImage = current.Cloudflare.WebhookImage
		wanted.Cloudflare = &cloudflare
	}
	wanted, err := platform.NormalizeNetworkSettings(wanted, manager.runtime.platformSettings.ClusterName)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	current, err = platform.NormalizeNetworkSettings(current, manager.runtime.platformSettings.ClusterName)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(wanted, current) {
		return managementNetworkConflict{}
	}
	return nil
}

func (manager Manager) reconcileManagementNetwork(ctx context.Context, settings platform.Settings, report func(string)) error {
	if settings.Network.Cloudflare != nil {
		credentials, err := manager.networkCredentials(ctx)
		if err != nil {
			return err
		}
		provider, err := cloudflare.New(cloudflare.Config{ZoneID: settings.Network.Cloudflare.ZoneID, ApiToken: credentials["cloudflareApiToken"]})
		if err != nil {
			return errors.New("management DNS provider is not configured")
		}
		if err := provider.ValidateZone(ctx, settings.Network.Cloudflare.WildcardDomain); err != nil {
			return errors.New("Cloudflare credentials cannot read the configured zone and DNS records")
		}
	}
	state, err := platform.ReconcileManagementNetwork(ctx, platform.ManagementInput{KubeconfigPath: manager.runtime.managementKubeconfigPath, Settings: settings, ReportProgress: report})
	if err != nil {
		return err
	}
	_, previous, err := loadDocument[platform.NetworkState](ctx, manager.database, "management-network-state", "shared")
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	write, err := documentWrite("management-network-state", "shared", state, previous.Revision)
	if err != nil {
		return err
	}
	return manager.database.Commit(ctx, []sqlite.Write{write})
}

func (manager Manager) networkCredentials(ctx context.Context) (map[string]string, error) {
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: manager.runtime.managementKubeconfigPath}, logger.Logger{})
	if err != nil {
		return nil, errors.New("management network connection unavailable")
	}
	namespace := manager.runtime.platformSettings.Namespace
	if namespace == "" {
		namespace = platform.ManagementNamespace
	}
	secret, err := connection.Secrets().Get(ctx, namespace, "borq-network-credentials")
	if err != nil {
		return nil, errors.New("management network credentials unavailable")
	}
	return secret.Data, nil
}

func (manager Manager) validateNetworkPocketIdName(address string) error {
	parsed, err := url.Parse(address)
	if err != nil {
		return fmt.Errorf("%w: invalid Pocket ID address", ErrInvalid)
	}
	settings, err := manager.GetPlatformSettings(manager.runtime.context)
	if err != nil {
		return err
	}
	if parsed.Hostname() == platform.ManagementPublicHostname(settings) {
		return fmt.Errorf("%w: hostname %s is reserved for the management server", ErrInvalid, parsed.Hostname())
	}
	return manager.validateNetworkHostname(parsed.Hostname(), "management")
}

func (manager Manager) validateNetworkHostname(host, scope string) error {
	current, err := manager.GetPlatformSettings(manager.runtime.context)
	if err != nil {
		return err
	}
	settings := current.Network
	if settings.Cloudflare == nil || !networkDnsPublicHost(host, settings.Cloudflare.WildcardDomain) {
		return nil
	}
	if scope == "management" {
		if networkDnsManagementHost(host, settings.Cloudflare.WildcardDomain) {
			return nil
		}
		return fmt.Errorf("%w: managed identity hostname must be one name below %s", ErrInvalid, settings.Cloudflare.WildcardDomain)
	}
	if !networkDnsPublicHost(host, scope+"."+settings.Cloudflare.WildcardDomain) {
		// DNS01 cannot distinguish a name from its wildcard authorization. Disjoint
		// subtrees keep that authority inside the requesting cluster's namespace.
		return fmt.Errorf("%w: managed hostname %s must be within %s.%s", ErrInvalid, host, scope, settings.Cloudflare.WildcardDomain)
	}
	return nil
}
