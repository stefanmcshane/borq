package management

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	localcluster "github.com/stefanmcshane/borq/business/cli/cluster"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func resolveBootstrapFiles(request Request, executable string) (Request, error) {
	request.inferredClusterName = request.ClusterName == ""
	local := request.KubeconfigPath == "" && !request.inferredClusterName
	if request.KubeconfigPath == "" && request.inferredClusterName {
		request.KubeconfigPath, request.localRuntime = "/etc/rancher/k3s/k3s.yaml", "native"
	} else if local {
		cluster, err := localcluster.GetLocal(request.ClusterName)
		if err != nil {
			return Request{}, err
		}
		request.KubeconfigPath = cluster.KubeconfigPath
		request.localRuntime = cluster.Runtime
		if _, err := os.Stat(request.KubeconfigPath); errors.Is(err, os.ErrNotExist) {
			if _, err := os.Stat("/etc/rancher/k3s/k3s.yaml"); err == nil {
				request.KubeconfigPath, request.localRuntime = "/etc/rancher/k3s/k3s.yaml", "native"
			}
		}
	} else {
		request.localRuntime = "external"
	}
	if _, err := os.Stat(request.KubeconfigPath); err != nil {
		return Request{}, fmt.Errorf("management Kubernetes must already be running; supply --kubeconfig for an existing cluster: %w", err)
	}
	configuration, err := clientcmd.LoadFromFile(request.KubeconfigPath)
	if err != nil {
		return Request{}, fmt.Errorf("read management kubeconfig: %w", err)
	}
	selected := configuration.Contexts[configuration.CurrentContext]
	if selected == nil || configuration.Clusters[selected.Cluster] == nil {
		return Request{}, errors.New("management kubeconfig must select a valid current context")
	}
	if request.inferredClusterName {
		request.ClusterName = bootstrapClusterName(selected.Cluster)
	}
	if strings.HasPrefix(selected.Cluster, "kind-") {
		request.localRuntime, request.localClusterName = "kind", strings.TrimPrefix(selected.Cluster, "kind-")
	} else if strings.HasPrefix(selected.Cluster, "k3d-") {
		request.localClusterName = strings.TrimPrefix(selected.Cluster, "k3d-")
	} else if request.localRuntime == "docker" {
		request.localClusterName = request.ClusterName
	}
	if request.Mode == "" {
		request.Mode = "prod"
		if request.localRuntime == "docker" {
			request.Mode = "dev"
		}
	}
	imageSelected := request.BorqImage != ""
	request.bootstrapProxy = true
	request = request.withDefaults()
	if (request.localRuntime == "docker" || request.localRuntime == "kind") && request.ServerImageArchivePath == "" && !imageSelected {
		archive := filepath.Join(filepath.Dir(executable), "borq-server-linux-"+runtime.GOARCH+".tar")
		if _, err := os.Stat(archive); err == nil {
			request.ServerImageArchivePath = archive
		} else if !errors.Is(err, os.ErrNotExist) {
			return Request{}, fmt.Errorf("read bundled server image: %w", err)
		}
	}
	if request.ServerImageArchivePath == "" {
		return request, nil
	}
	image, err := serverArchiveImage(request.ServerImageArchivePath, request.BorqImage)
	if err != nil {
		return Request{}, err
	}
	request.BorqImage = image
	return request, nil
}

func importServerImage(ctx context.Context, request Request) error {
	if request.localClusterName == "" {
		return errors.New("offline image import requires a local kind or k3d cluster; publish the image and use --server-image for other clusters")
	}
	if request.localRuntime != "kind" {
		return localcluster.ImportImage(ctx, request.localClusterName, request.ServerImageArchivePath)
	}
	// A context name alone is not enough: it could describe a remote kind cluster
	// with the same name. Verify the local cluster's CA before importing anything.
	encoded, err := exec.CommandContext(ctx, "kind", "get", "kubeconfig", "--name", request.localClusterName).Output()
	if err != nil {
		return fmt.Errorf("read local kind cluster (kind must be installed): %w", err)
	}
	local, err := clientcmd.Load(encoded)
	if err != nil {
		return errors.New("local kind returned an invalid kubeconfig")
	}
	selected, err := clientcmd.LoadFromFile(request.KubeconfigPath)
	if err != nil {
		return err
	}
	for _, configuration := range []*clientcmdapi.Config{local, selected} {
		if err := clientcmdapi.MinifyConfig(configuration); err != nil {
			return fmt.Errorf("resolve kind context: %w", err)
		}
		if err := clientcmdapi.FlattenConfig(configuration); err != nil {
			return fmt.Errorf("resolve kind certificate: %w", err)
		}
	}
	localCa := local.Clusters[local.Contexts[local.CurrentContext].Cluster].CertificateAuthorityData
	selectedCa := selected.Clusters[selected.Contexts[selected.CurrentContext].Cluster].CertificateAuthorityData
	if len(localCa) == 0 || !bytes.Equal(localCa, selectedCa) {
		return errors.New("selected Kubernetes cluster does not match the local kind cluster; use a registry image for remote clusters")
	}
	if err := exec.CommandContext(ctx, "kind", "load", "image-archive", request.ServerImageArchivePath, "--name", request.localClusterName).Run(); err != nil {
		return fmt.Errorf("load server image into kind: %w", err)
	}
	return nil
}

func serverArchiveImage(path, selected string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open server image archive: %w", err)
	}
	defer file.Close()
	archive := tar.NewReader(file)
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			return "", errors.New("server image archive has no Docker manifest.json")
		}
		if err != nil {
			return "", fmt.Errorf("read server image archive: %w", err)
		}
		if filepath.Clean(header.Name) != "manifest.json" {
			continue
		}
		var manifest []struct{ RepoTags []string }
		if header.Size > 1<<20 {
			return "", errors.New("server image manifest exceeds 1MiB")
		}
		if err := json.NewDecoder(io.LimitReader(archive, 1<<20)).Decode(&manifest); err != nil {
			return "", fmt.Errorf("read server image manifest: %w", err)
		}
		for _, image := range manifest {
			if slices.Contains(image.RepoTags, selected) {
				return selected, nil
			}
		}
		if selected == borqImage && len(manifest) == 1 && len(manifest[0].RepoTags) == 1 {
			return manifest[0].RepoTags[0], nil
		}
		return "", fmt.Errorf("server image archive does not identify selected image %q; use --server-image to select a contained tag", selected)
	}
}

// Kubeconfig cluster keys can be cloud-provider ARNs rather than DNS labels.
// Preserve simple names and use a digest when normalization could merge names.
func bootstrapClusterName(cluster string) string {
	name := strings.TrimPrefix(strings.TrimPrefix(cluster, "kind-"), "k3d-")
	if len(validation.IsDNS1123Label(name)) == 0 && len(name) <= 58 {
		return name
	}
	name = strings.Map(func(character rune) rune {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '-' {
			return character
		}
		return '-'
	}, strings.ToLower(name))
	name = strings.Trim(name, "-")
	if name == "" {
		name = "cluster"
	}
	if len(name) > 45 {
		name = strings.TrimRight(name[:45], "-")
	}
	digest := sha256.Sum256([]byte(cluster))
	return fmt.Sprintf("%s-%x", name, digest[:6])
}
