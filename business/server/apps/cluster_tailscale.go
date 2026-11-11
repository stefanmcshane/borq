package apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/stefanmcshane/borq/business/server/platform"
)

func validateClusterTailscale(input ClusterInput) error {
	if input.Tailscale == nil {
		return nil
	}
	credentials := *input.Tailscale
	if credentials.CredentialsRef != "" {
		if credentials.ClientID != "" || credentials.ClientSecret != "" || !validClusterTailscaleRef(platform.ClusterID(input.Organization, input.Name), credentials.CredentialsRef) {
			return fmt.Errorf("%w: Tailscale reference must belong to this cluster and cannot accompany inline credentials", ErrInvalid)
		}
		return nil
	}
	if strings.TrimSpace(credentials.ClientID) == "" || strings.TrimSpace(credentials.ClientSecret) == "" {
		return fmt.Errorf("%w: cluster Tailscale requires both clientId and clientSecret", ErrInvalid)
	}
	return nil
}

func validClusterTailscaleRef(id, ref string) bool {
	prefix := "tailscale-" + id + "-"
	version, ok := strings.CutPrefix(ref, prefix)
	return ok && len(id) == 32 && strings.Trim(id, "0123456789abcdef") == "" && len(version) == 32 && strings.Trim(version, "0123456789abcdef") == ""
}

func (manager Manager) readClusterTailscale(cluster Cluster) (ClusterTailscale, error) {
	if cluster.Tailscale == nil || !validClusterTailscaleRef(cluster.ID, cluster.Tailscale.CredentialsRef) {
		return ClusterTailscale{}, fmt.Errorf("%w: private Tailscale credentials are required for this cluster", ErrInvalid)
	}
	path := filepath.Join(manager.runtime.credentialsDir, cluster.Tailscale.CredentialsRef+".json")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return ClusterTailscale{}, errors.New("private cluster Tailscale credentials are unavailable")
	}
	encoded, err := os.ReadFile(path)
	if err != nil {
		return ClusterTailscale{}, errors.New("read private cluster Tailscale credentials")
	}
	var credentials ClusterTailscale
	if json.Unmarshal(encoded, &credentials) != nil || credentials.CredentialsRef != "" || strings.TrimSpace(credentials.ClientID) == "" || strings.TrimSpace(credentials.ClientSecret) == "" {
		return ClusterTailscale{}, errors.New("private cluster Tailscale credential document is invalid")
	}
	return credentials, nil
}

func (manager Manager) stageClusterTailscale(input ClusterInput, previous Cluster) (ClusterInput, string, error) {
	if input.Tailscale == nil {
		input.Tailscale = previous.Tailscale
		return input, "", nil
	}
	if err := validateClusterTailscale(input); err != nil {
		return ClusterInput{}, "", err
	}
	if input.Tailscale.CredentialsRef != "" {
		_, err := manager.readClusterTailscale(input)
		return input, "", err
	}
	credentials := *input.Tailscale
	if previous.Tailscale != nil {
		stored, err := manager.readClusterTailscale(previous)
		if err == nil && stored.ClientID == credentials.ClientID && stored.ClientSecret == credentials.ClientSecret {
			input.Tailscale = previous.Tailscale
			return input, "", nil
		}
	}
	version, err := newId()
	if err != nil {
		return ClusterInput{}, "", err
	}
	ref := "tailscale-" + input.ID + "-" + version
	if err := os.MkdirAll(manager.runtime.credentialsDir, 0o700); err != nil {
		return ClusterInput{}, "", errors.New("create private cluster credential directory")
	}
	encoded, err := json.Marshal(credentials)
	if err != nil {
		return ClusterInput{}, "", errors.New("encode private cluster credentials")
	}
	path := filepath.Join(manager.runtime.credentialsDir, ref+".json")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return ClusterInput{}, "", errors.New("create private cluster credentials")
	}
	_, writeErr := file.Write(encoded)
	syncErr := file.Sync()
	closeErr := file.Close()
	if errors.Join(writeErr, syncErr, closeErr) != nil {
		_ = os.Remove(path)
		return ClusterInput{}, "", errors.New("save private cluster credentials")
	}
	directory, err := os.Open(manager.runtime.credentialsDir)
	if err != nil {
		_ = os.Remove(path)
		return ClusterInput{}, "", errors.New("open private credential directory for synchronization")
	}
	directorySyncErr, directoryCloseErr := directory.Sync(), directory.Close()
	if errors.Join(directorySyncErr, directoryCloseErr) != nil {
		_ = os.Remove(path)
		return ClusterInput{}, "", errors.New("synchronize private credential directory")
	}
	input.Tailscale = &ClusterTailscale{CredentialsRef: ref}
	input.tailscaleStagedRef = ref
	return input, ref, nil
}

func (manager Manager) cleanClusterTailscale(cluster Cluster, ref string) {
	if !validClusterTailscaleRef(platform.ClusterID(cluster.Organization, cluster.Name), ref) {
		return
	}
	current, _, err := loadDocument[Cluster](context.Background(), manager.database, "cluster", cluster.Organization+"/"+cluster.Name)
	if errors.Is(err, ErrNotFound) || (err == nil && (current.Tailscale == nil || current.Tailscale.CredentialsRef != ref)) {
		_ = os.Remove(filepath.Join(manager.runtime.credentialsDir, ref+".json"))
	}
}

// Management credentials require a server-owned connection as well as physical
// identity: an uploaded cluster API can imitate management's namespace UID.
func (manager Manager) clusterPlatformSettings(ctx context.Context, cluster Cluster, settings platform.Settings) (platform.Settings, bool, error) {
	if settings.Network.Tailscale != nil || manager.runtime.managementBootstrapOnly {
		converged, err := manager.applicationOnManagementCluster(ctx, cluster)
		if err != nil {
			return platform.Settings{}, false, err
		}
		if converged {
			// Only a target in this physical cluster can use management Service DNS.
			// A fresh bootstrap has no node address from which to derive endpoints.
			namespace := settings.Namespace
			if namespace == "" {
				namespace = platform.ManagementNamespace
			}
			if settings.OpenBaoURL == "" {
				settings.OpenBaoURL = "http://openbao." + namespace + ".svc:8200"
			}
			if settings.TelemetryURL == "" && settings.LokiURL == "" && settings.PrometheusURL == "" {
				settings.TelemetryURL = "http://borq." + namespace + ".svc:80"
			}
			if settings.S3Endpoint == "" {
				settings.S3Endpoint = "http://rustfs-svc." + namespace + ".svc:9000"
			}
			return settings, true, nil
		}
	}
	// Management DNS credentials and issuer scope never become remote defaults.
	settings.Network.Cloudflare = nil
	if cluster.Tailscale == nil {
		settings.Network.Tailscale = nil
		return settings, false, nil
	}
	if settings.Network.Tailscale == nil {
		settings.Network.Tailscale = &platform.TailscaleSettings{}
	}
	clusterName := settings.ClusterName
	if clusterName == "" {
		clusterName = "management"
	}
	network, err := platform.NormalizeNetworkSettings(settings.Network, clusterName)
	settings.Network = network
	return settings, false, err
}

func (manager Manager) clusterOperatorCredentials(ctx context.Context, cluster Cluster, converged bool) (ClusterTailscale, error) {
	if converged {
		credentials, err := manager.networkCredentials(ctx)
		if err != nil {
			return ClusterTailscale{}, err
		}
		return ClusterTailscale{ClientID: credentials["tailscaleClientId"], ClientSecret: credentials["tailscaleClientSecret"]}, nil
	}
	credentials, err := manager.readClusterTailscale(cluster)
	if err != nil {
		return ClusterTailscale{}, err
	}
	settings, err := manager.GetPlatformSettings(ctx)
	if err != nil {
		return ClusterTailscale{}, err
	}
	if settings.Network.Tailscale != nil {
		management, err := manager.networkCredentials(ctx)
		if err != nil {
			return ClusterTailscale{}, err
		}
		if credentials.ClientID == management["tailscaleClientId"] {
			return ClusterTailscale{}, fmt.Errorf("%w: a remote cluster must use a separate Tailscale OAuth application from management", ErrInvalid)
		}
	}
	return credentials, nil
}

func (manager Manager) registerManagementCluster(ctx context.Context, cluster ClusterInput, base string, captured bool) (Cluster, error) {
	manager.runtime.registrationMutex.Lock()
	defer manager.runtime.registrationMutex.Unlock()
	if cluster.Tailscale != nil {
		if !captured {
			return Cluster{}, fmt.Errorf("%w: management cluster credential snapshot is missing", ErrConflict)
		}
		current, _, err := loadDocument[Cluster](ctx, manager.database, "cluster", cluster.Organization+"/"+cluster.Name)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return Cluster{}, err
		}
		currentRef := ""
		if current.Tailscale != nil {
			currentRef = current.Tailscale.CredentialsRef
		}
		if currentRef != base && currentRef != cluster.Tailscale.CredentialsRef {
			return Cluster{}, fmt.Errorf("%w: cluster Tailscale credentials changed after management configuration was accepted", ErrConflict)
		}
	}
	return manager.registerCluster(ctx, cluster, nil)
}

func managementClusterTailscaleBases(configuration ManagementConfiguration) map[string]string {
	bases := map[string]string{}
	for _, organization := range configuration.Organizations {
		for _, cluster := range organization.Clusters {
			if cluster.tailscaleBaseCaptured {
				bases[organization.Name+"/"+cluster.Name] = cluster.tailscaleBaseRef
			}
		}
	}
	return bases
}
