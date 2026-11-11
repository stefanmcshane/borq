package cluster

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/gofrs/flock"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/yaml"
)

// Local records the provisioning runtime and credential location for a named local cluster.
type Local struct {
	// Organization records the selected owner; older local records may omit it.
	Organization string `json:"organization,omitempty"`
	// Name identifies the local cluster and its private credential file.
	Name string `json:"name"`
	// Runtime is docker or native and determines local component defaults.
	Runtime string `json:"runtime"`
	// KubeconfigPath is the actual private credential file selected during creation.
	KubeconfigPath string `json:"kubeconfigPath"`
}

// GetLocal reads the shared local registry, importing older per-cluster records.
// Unregistered names use native k3s and their conventional kubeconfig location.
func GetLocal(name string) (Local, error) {
	path, err := LocalKubeconfigPath(name)
	if err != nil {
		return Local{}, err
	}
	registry := filepath.Join(filepath.Dir(filepath.Dir(path)), "clusters.yaml")
	lock, err := lockLocalRegistry(registry)
	if err != nil {
		return Local{}, err
	}
	defer lock.Close()
	clusters, migrated, err := readLocalRegistry(registry)
	if err != nil {
		return Local{}, err
	}
	if migrated {
		if err := writeLocalRegistry(registry, clusters); err != nil {
			return Local{}, err
		}
	}
	for _, cluster := range clusters {
		if cluster.Name == name {
			return cluster, nil
		}
	}
	return Local{Name: name, Runtime: "native", KubeconfigPath: path}, nil
}

func (local Local) validate() error {
	if err := validateClusterName(local.Name); err != nil {
		return err
	}
	if local.Organization != "" {
		if err := validateClusterName(local.Organization); err != nil {
			return fmt.Errorf("local cluster metadata has an invalid organization")
		}
	}
	if (local.Runtime != "docker" && local.Runtime != "native") || local.KubeconfigPath == "" {
		return fmt.Errorf("local cluster metadata has an invalid runtime or kubeconfig path")
	}
	return nil
}

// flock requires a pointer to retain the operating system lock until Close.
func lockLocalRegistry(path string) (*flock.Flock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("create local cluster registry directory: %w", err)
	}
	lock := flock.New(path + ".lock")
	if err := lock.Lock(); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("lock local cluster registry: %w", err)
	}
	return lock, nil
}

func readLocalRegistryFile(path string) ([]Local, error) {
	encoded, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return []Local{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read local cluster registry: %w", err)
	}
	var clusters []Local
	if err := yaml.UnmarshalStrict(encoded, &clusters); err != nil || clusters == nil {
		return nil, fmt.Errorf("local cluster registry %s must contain a YAML list of clusters", path)
	}
	known := map[string]bool{}
	for _, cluster := range clusters {
		if err := cluster.validate(); err != nil {
			return nil, fmt.Errorf("invalid local cluster registry: %w", err)
		}
		if known[cluster.Name] {
			return nil, fmt.Errorf("local cluster registry repeats cluster %q", cluster.Name)
		}
		known[cluster.Name] = true
	}
	return clusters, nil
}

func readLocalRegistry(path string) ([]Local, bool, error) {
	_, currentErr := os.Stat(path)
	clusters, err := readLocalRegistryFile(path)
	if err != nil {
		return nil, false, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, false, err
	}
	root, legacyRoot := filepath.Dir(path), filepath.Join(home, ".borq")
	known := map[string]bool{}
	migrated := false
	for index, cluster := range clusters {
		local, err := relocateLocal(cluster, legacyRoot, root)
		if err != nil {
			return nil, false, err
		}
		clusters[index] = local
		migrated = migrated || local.KubeconfigPath != cluster.KubeconfigPath
		known[local.Name] = true
	}
	for _, sourceRoot := range []string{root, legacyRoot} {
		if sourceRoot != root && errors.Is(currentErr, os.ErrNotExist) {
			previous, err := readLocalRegistryFile(filepath.Join(sourceRoot, "clusters.yaml"))
			if err != nil {
				return nil, false, err
			}
			for _, local := range previous {
				if known[local.Name] {
					continue
				}
				local, err = relocateLocal(local, sourceRoot, root)
				if err != nil {
					return nil, false, err
				}
				clusters = append(clusters, local)
				known[local.Name], migrated = true, true
			}
		}
		directory := filepath.Join(sourceRoot, "clusters")
		entries, err := os.ReadDir(directory)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, false, fmt.Errorf("read legacy local clusters: %w", err)
		}
		for _, entry := range entries {
			if !entry.IsDir() || known[entry.Name()] {
				continue
			}
			legacyPath := filepath.Join(directory, entry.Name(), "kubeconfig")
			local := Local{Name: entry.Name(), Runtime: "docker", KubeconfigPath: legacyPath}
			metadata, err := os.ReadFile(filepath.Join(directory, entry.Name(), "cluster.yaml"))
			if errors.Is(err, os.ErrNotExist) {
				if _, err := os.Stat(legacyPath); errors.Is(err, os.ErrNotExist) {
					continue
				} else if err != nil {
					return nil, false, fmt.Errorf("read legacy kubeconfig: %w", err)
				}
			} else if err != nil {
				return nil, false, fmt.Errorf("read legacy cluster metadata: %w", err)
			} else {
				local = Local{}
				if err := yaml.UnmarshalStrict(metadata, &local); err != nil {
					return nil, false, fmt.Errorf("invalid legacy cluster metadata for %q", entry.Name())
				}
			}
			if local.Name != entry.Name() {
				return nil, false, fmt.Errorf("legacy cluster metadata for %q has a different name", entry.Name())
			}
			if err := local.validate(); err != nil {
				return nil, false, err
			}
			local, err = relocateLocal(local, sourceRoot, root)
			if err != nil {
				return nil, false, err
			}
			clusters = append(clusters, local)
			known[local.Name], migrated = true, true
		}
	}
	return clusters, migrated, nil
}

func relocateLocal(local Local, sourceRoot, destinationRoot string) (Local, error) {
	if local.KubeconfigPath != filepath.Join(sourceRoot, "kubeconfigs", local.Name) &&
		local.KubeconfigPath != filepath.Join(sourceRoot, "clusters", local.Name, "kubeconfig") {
		return local, nil
	}
	destination := filepath.Join(destinationRoot, "kubeconfigs", local.Name)
	if local.KubeconfigPath == destination {
		return local, nil
	}
	if err := migrateLocalKubeconfig(local.KubeconfigPath, destination); err != nil {
		return Local{}, err
	}
	local.KubeconfigPath = destination
	return local, nil
}

func migrateLocalKubeconfig(source, destination string) error {
	configuration, err := clientcmd.LoadFromFile(source)
	if errors.Is(err, os.ErrNotExist) {
		return nil // Keep missing legacy credentials discoverable so callers can supply an existing kubeconfig.
	}
	if err != nil {
		return fmt.Errorf("read legacy kubeconfig %s: %w", source, err)
	}
	if err := clientcmd.ResolveLocalPaths(configuration); err != nil {
		return fmt.Errorf("resolve legacy kubeconfig file paths: %w", err)
	}
	for _, authentication := range configuration.AuthInfos {
		if authentication.TokenFile == "" {
			continue
		}
		token, err := os.ReadFile(authentication.TokenFile)
		if err != nil {
			return fmt.Errorf("embed migrated kubeconfig token: %w", err)
		}
		authentication.Token, authentication.TokenFile = strings.TrimSpace(string(token)), ""
	}
	if err := clientcmdapi.FlattenConfig(configuration); err != nil {
		return fmt.Errorf("make migrated kubeconfig self-contained: %w", err)
	}
	contents, err := clientcmd.Write(*configuration)
	if err != nil {
		return fmt.Errorf("encode migrated kubeconfig: %w", err)
	}
	current, err := os.ReadFile(destination)
	if err == nil {
		if !bytes.Equal(current, contents) {
			return fmt.Errorf("cannot migrate cluster credentials: %s already exists with different contents", destination)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read migrated kubeconfig: %w", err)
	}
	return writePrivateFile(destination, contents)
}

func writeLocalRegistry(path string, clusters []Local) error {
	slices.SortFunc(clusters, func(left, right Local) int { return strings.Compare(left.Name, right.Name) })
	encoded, err := yaml.Marshal(clusters)
	if err != nil {
		return fmt.Errorf("encode local cluster registry: %w", err)
	}
	if err := writePrivateFile(path, encoded); err != nil {
		return fmt.Errorf("save local cluster registry: %w", err)
	}
	return nil
}
