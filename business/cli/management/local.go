package management

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"

	localcluster "github.com/stefanmcshane/borq/business/cli/cluster"
)

func resolveBootstrapFiles(request Request, executable string) (Request, error) {
	if request.ClusterName == "" {
		request.ClusterName = defaultClusterName
	}
	local := request.KubeconfigPath == ""
	if local {
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
	if request.Mode == "" {
		request.Mode = "prod"
		if request.localRuntime == "docker" {
			request.Mode = "dev"
		}
	}
	imageSelected := request.BorqImage != ""
	request = request.withDefaults()
	if request.localRuntime == "docker" && request.ServerImageArchivePath == "" && !imageSelected {
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
