package apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"slices"
	"strings"

	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
)

// ApplicationRoutePolicy exposes public suffix choices from the space's pinned issuer.
type ApplicationRoutePolicy struct {
	// CertificateIssuer is selected by the space, never by an application request.
	CertificateIssuer string `json:"certificateIssuer"`
	// Domains permits exactly one additional DNS label per public route.
	Domains []string `json:"domains"`
}

type applicationRouteError struct{ message string }

func (err applicationRouteError) Error() string { return err.message }
func (err applicationRouteError) Unwrap() error { return ErrInvalid }

// ApplicationRequestMessage exposes only deliberate, credential-free route guidance.
func ApplicationRequestMessage(err error) string {
	var route applicationRouteError
	if errors.As(err, &route) {
		return route.message
	}
	return ""
}

// GetApplicationRoutePolicy reads only the issuer pinned to this space's registered cluster.
func (manager Manager) GetApplicationRoutePolicy(ctx context.Context, organization, name, application string) (ApplicationRoutePolicy, error) {
	if application != "" {
		if err := validateNames(application); err != nil {
			return ApplicationRoutePolicy{}, applicationRouteError{"Enter a valid application name before choosing a public route."}
		}
	}
	stored, _, err := loadDocument[spaceRecord](ctx, manager.database, "space", spaceKey(organization, name))
	if err != nil {
		return ApplicationRoutePolicy{}, err
	}
	policy, err := manager.applicationIssuerPolicy(ctx, stored.Space, application)
	return ApplicationRoutePolicy{CertificateIssuer: policy.CertificateIssuer, Domains: policy.Domains}, err
}

func (manager Manager) applicationIssuerPolicy(ctx context.Context, space Space, application string) (platform.ApplicationRoutePolicy, error) {
	cluster, _, err := loadDocument[Cluster](ctx, manager.database, "cluster", space.Organization+"/"+space.Cluster)
	if err != nil {
		return platform.ApplicationRoutePolicy{}, err
	}
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: cluster.KubeconfigPath, Context: cluster.Context, InCluster: cluster.InCluster}, logger.Logger{})
	if err != nil {
		return platform.ApplicationRoutePolicy{}, applicationRouteError{"The space's cluster is unavailable; public route choices could not be read."}
	}
	labels := map[string]string{"app.kubernetes.io/name": application, "app.kubernetes.io/managed-by": "borq", "borq.run/target-id": space.ID,
		"borq.run/space-id": space.ID, "borq.run/organization-id": space.Organization, "borq.run/organization": space.Organization,
		"borq.run/space": space.Name, "borq.run/cluster-id": cluster.ID, "borq.run/cluster": space.Cluster,
		"borq.run/app-name": application, "borq.run/app-id": stableId(space.ID + "/" + application)}
	policy, err := platform.ReadApplicationRoutePolicy(ctx, connection, space.CertificateIssuer, labels)
	if err != nil {
		return policy, applicationRouteError{"The space's pinned ClusterIssuer is unavailable. Configure its explicit DNS zones before adding public routes."}
	}
	settings, err := manager.GetPlatformSettings(ctx)
	if err != nil || settings.Network.Cloudflare == nil {
		return policy, err
	}
	local, err := manager.applicationOnManagementCluster(ctx, cluster)
	if err != nil {
		return policy, err
	}
	if local {
		return policy, nil
	}
	// A remote cluster administrator can edit its issuer; that cannot grant
	// authority over management's DNS zone or another registered cluster's names.
	managed := settings.Network.Cloudflare.WildcardDomain
	delegated := cluster.ID + "." + managed
	domains := []string{}
	for _, domain := range policy.Domains {
		switch {
		case domain == managed || strings.HasSuffix(domain, "."+managed) && (domain != delegated && !strings.HasSuffix(domain, "."+delegated)):
			if delegated == domain || strings.HasSuffix(delegated, "."+domain) {
				domains = append(domains, delegated)
			}
		default:
			domains = append(domains, domain)
		}
	}
	slices.Sort(domains)
	policy.Domains = slices.Compact(domains)
	return policy, nil
}

func (manager Manager) applicationOnManagementCluster(ctx context.Context, cluster Cluster) (bool, error) {
	// Only server-owned connection choices can establish management authority.
	// A remote API can imitate a namespace UID, so UID equality alone is insufficient.
	trusted := cluster.InCluster || cluster.KubeconfigPath != "" && manager.runtime.managementKubeconfigPath != "" &&
		filepath.Clean(cluster.KubeconfigPath) == filepath.Clean(manager.runtime.managementKubeconfigPath) && cluster.Context == ""
	if !trusted {
		return false, nil
	}
	return samePhysicalCluster(ctx, Cluster{KubeconfigPath: manager.runtime.managementKubeconfigPath}, cluster)
}

func (manager Manager) applicationTunnelDomain(ctx context.Context, space Space) (string, error) {
	settings, err := manager.GetPlatformSettings(ctx)
	if err != nil || settings.Network.Cloudflare == nil || settings.Network.Cloudflare.ExposureMode() != "tunnel" {
		return "", err
	}
	cluster, _, err := loadDocument[Cluster](ctx, manager.database, "cluster", space.Organization+"/"+space.Cluster)
	if err != nil {
		return "", err
	}
	local, err := manager.applicationOnManagementCluster(ctx, cluster)
	if err != nil || !local {
		return "", err
	}
	return settings.Network.Cloudflare.WildcardDomain, nil
}

func (manager Manager) validateApplicationRoutes(ctx context.Context, space Space, configuration Configuration) error {
	for _, application := range configuration.Apps {
		if len(application.Ingresses) == 0 {
			continue
		}
		policy, err := manager.applicationIssuerPolicy(ctx, space, application.Name)
		if err != nil {
			return err
		}
		for _, route := range application.Ingresses {
			if err := platform.ValidateApplicationRouteHost(policy, route.Host); err != nil {
				return applicationRouteError{fmt.Sprintf("Public routes must use one subdomain label under a domain permitted by the space's ClusterIssuer %s: %s.", policy.CertificateIssuer, strings.Join(policy.Domains, ", "))}
			}
			if err := manager.validateApplicationDnsAuthority(ctx, space, route.Host); err != nil {
				return err
			}
		}
	}
	return manager.validateApplicationRouteOwners(ctx, space, configuration)
}

func (manager Manager) validateApplicationDnsAuthority(ctx context.Context, space Space, host string) error {
	settings, err := manager.GetPlatformSettings(ctx)
	if err != nil || settings.Network.Cloudflare == nil || !networkDnsPublicHost(host, settings.Network.Cloudflare.WildcardDomain) {
		return err
	}
	cluster, _, err := loadDocument[Cluster](ctx, manager.database, "cluster", space.Organization+"/"+space.Cluster)
	if err != nil {
		return err
	}
	local, err := manager.applicationOnManagementCluster(ctx, cluster)
	if err != nil || local {
		return err
	}
	domain := cluster.ID + "." + settings.Network.Cloudflare.WildcardDomain
	if !networkDnsPublicHost(host, domain) {
		return applicationRouteError{"This remote cluster may publish management-zone routes only beneath " + domain + "."}
	}
	return nil
}

func (manager Manager) validateApplicationRouteOwners(ctx context.Context, space Space, configuration Configuration) error {
	hosts := map[string]bool{}
	for _, application := range configuration.Apps {
		for _, route := range application.Ingresses {
			hosts[route.Host] = true
		}
	}
	if len(hosts) == 0 {
		return nil
	}
	for host := range hosts {
		if err := manager.checkApplicationHostnameOwner(ctx, host, space.ID); err != nil {
			return err
		}
	}
	return nil
}

func (manager Manager) checkApplicationHostnameOwner(ctx context.Context, host, spaceID string) error {
	settings, err := manager.GetPlatformSettings(ctx)
	if err != nil {
		return err
	}
	if applicationHostnameOverlap(host, platform.ManagementPublicHostname(settings)) {
		return applicationRouteError{"This hostname is reserved for the Borq server. Choose another subdomain."}
	}
	reservations, err := manager.database.List(ctx, "organization-pocketid-host")
	if err != nil {
		return err
	}
	for _, reservation := range reservations {
		if applicationHostnameOverlap(host, reservation.Key) {
			return applicationRouteError{"This hostname is reserved for Pocket ID. Choose another subdomain."}
		}
	}
	providers, err := manager.database.List(ctx, "organization-pocketid")
	if err != nil {
		return err
	}
	for _, record := range providers {
		var provider organizationPocketIdRecord
		if err := json.Unmarshal(record.Value, &provider); err != nil {
			return err
		}
		address, err := url.Parse(provider.Status.IssuerURL)
		if err == nil && applicationHostnameOverlap(host, address.Hostname()) {
			return applicationRouteError{"This hostname is reserved for Pocket ID. Choose another subdomain."}
		}
	}
	tasks, err := manager.database.List(ctx, "management-task")
	if err != nil {
		return err
	}
	for _, record := range tasks {
		operation, err := manager.GetManagementOperation(ctx, record.Key)
		if err != nil {
			return err
		}
		if operationFinished(operation.Status) {
			continue
		}
		var task managementTask
		if err := json.Unmarshal(record.Value, &task); err != nil {
			return err
		}
		for _, reserved := range append(task.Hostnames, managementRouteHostnames(task.Configuration)...) {
			if applicationHostnameOverlap(host, reserved) {
				return applicationRouteError{"This hostname is reserved by management setup. Choose another subdomain."}
			}
		}
	}
	return manager.checkAcceptedApplicationHostname(ctx, host, spaceID)
}

func (manager Manager) checkAcceptedApplicationHostname(ctx context.Context, host, spaceID string) error {
	spaces, err := manager.database.List(ctx, "space")
	if err != nil {
		return err
	}
	owners := map[string]spaceRecord{}
	for _, record := range spaces {
		var other spaceRecord
		if err := json.Unmarshal(record.Value, &other); err != nil {
			return err
		}
		if other.Space.ID == spaceID {
			continue
		}
		owners[other.Space.ID] = other
		if applicationConfigurationHasHost(other.Space.Configuration, host) {
			return applicationRouteError{"This hostname is already assigned to another space. Choose another subdomain."}
		}
	}
	// Accepted revisions can leave a live route even after a failed or canceled
	// rollout. Keep its hostname until a later successful revision replaces it.
	operations, err := manager.database.List(ctx, "operation")
	if err != nil {
		return err
	}
	for _, record := range operations {
		var operation Operation
		if err := json.Unmarshal(record.Value, &operation); err != nil {
			return err
		}
		owner, exists := owners[operation.SpaceID]
		if !exists || operation.PlanID == "" {
			continue
		}
		plan, _, err := loadDocument[planRecord](ctx, manager.database, "plan", operation.PlanID)
		if err != nil {
			return err
		}
		if !operationFinished(operation.Status) || plan.Plan.BaseRevision+1 >= owner.Space.LastSuccessfulRevision {
			if applicationConfigurationHasHost(plan.Plan.Configuration, host) {
				return applicationRouteError{"This hostname is still assigned to another space's deployed or unfinished revision. Choose another subdomain."}
			}
		}
	}
	return nil
}

func applicationConfigurationHasHost(configuration Configuration, host string) bool {
	for _, application := range configuration.Apps {
		for _, route := range application.Ingresses {
			if applicationHostnameOverlap(host, route.Host) {
				return true
			}
		}
	}
	return false
}

func managementRouteHostnames(configuration ManagementConfiguration) []string {
	result := []string{}
	if address, err := url.Parse(configuration.BorqPublicURL); err == nil && address.Hostname() != "" {
		result = append(result, address.Hostname())
	}
	for _, organization := range configuration.Organizations {
		provider := organization.ManagedPocketId
		if provider == nil {
			continue
		}
		if address, err := url.Parse(provider.IssuerURL); err == nil && address.Hostname() != "" {
			result = append(result, address.Hostname())
		}
	}
	return result
}

func (manager Manager) validateManagementApplicationHostnames(ctx context.Context, configuration ManagementConfiguration) error {
	for _, host := range managementRouteHostnames(configuration) {
		if err := manager.checkAcceptedApplicationHostname(ctx, host, ""); err != nil {
			return err
		}
	}
	return nil
}

func applicationHostnameOverlap(first, second string) bool {
	if first == "" || second == "" {
		return false
	}
	if first == second {
		return true
	}
	for _, pair := range [][2]string{{first, second}, {second, first}} {
		if strings.HasPrefix(pair[0], "*.") && strings.HasSuffix(pair[1], pair[0][1:]) {
			label := strings.TrimSuffix(pair[1], pair[0][1:])
			if label != "" {
				return true
			}
		}
	}
	return false
}
