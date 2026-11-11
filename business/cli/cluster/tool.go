package cluster

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/stefanmcshane/borq/business/cli/localconfig"
)

type k3dAsset struct{ name, checksum string }

func privateK3d(ctx context.Context) (string, error) {
	if binary, err := exec.LookPath("k3d"); err == nil {
		return binary, nil
	}
	local := filepath.Join("tmp", "tools", "k3d")
	if info, err := os.Stat(local); err == nil && info.Mode().IsRegular() && info.Mode()&0111 != 0 {
		return local, nil
	}
	directory, err := localconfig.Directory()
	if err != nil {
		return "", fmt.Errorf("find private k3d directory: %w", err)
	}
	destination := filepath.Join(directory, "bin", "k3d")
	if info, err := os.Stat(destination); err == nil && info.Mode().IsRegular() && info.Mode()&0111 != 0 {
		return destination, nil
	}
	asset, err := k3dAssetForPlatform(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return "", err
	}
	// Reuse an older private install only when it matches the currently pinned binary.
	if home, err := os.UserHomeDir(); err == nil {
		if contents, err := os.ReadFile(filepath.Join(home, ".borq", "bin", "k3d")); err == nil {
			digest := sha256.Sum256(contents)
			if hex.EncodeToString(digest[:]) == asset.checksum {
				if err := writePrivateFile(destination, contents); err != nil {
					return "", err
				}
				if err := os.Chmod(destination, 0700); err != nil {
					return "", err
				}
				return destination, nil
			}
		}
	}
	if err := downloadK3d(ctx, http.Client{Timeout: 2 * time.Minute}, "https://github.com/k3d-io/k3d/releases/download/"+defaultK3dVersion+"/"+asset.name, destination, asset.checksum); err != nil {
		return "", err
	}
	return destination, nil
}

func k3dAssetForPlatform(system, architecture string) (k3dAsset, error) {
	// Checksums are pinned from the upstream v5.8.3 checksums.txt release asset.
	checksums := map[string]string{
		"darwin-amd64": "fd0f8e9e8ea4d8bc3674572ca6ed0833b639bf57c43c708616d937377324cfea",
		"darwin-arm64": "8da468daa7dc7cf7cdd4735f90a9bb05179fa27858250f62e3d8cdf5b5ca0698",
		"linux-amd64":  "dbaa79a76ace7f4ca230a1ff41dc7d8a5036a8ad0309e9c54f9bf3836dbe853e",
		"linux-arm64":  "0b8110f2229631af7402fb828259330985918b08fefd38b7f1b788a1c8687216",
		"linux-arm":    "c4ef4e8008edb55cf347d846a7fc70af319883f9a474167689bd8af04693401d",
		"linux-386":    "0f48e31c2ada50bd99f133b9fb19edfbba2cabce589667a1fe628a7208f721ac",
	}
	key := system + "-" + architecture
	checksum, ok := checksums[key]
	if !ok {
		return k3dAsset{}, fmt.Errorf("private k3d installation is not supported on %s", key)
	}
	return k3dAsset{name: "k3d-" + key, checksum: checksum}, nil
}

func downloadK3d(ctx context.Context, client http.Client, address, destination, checksum string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return fmt.Errorf("prepare private k3d download: %w", err)
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("download private k3d: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("download private k3d: HTTP %d", response.StatusCode)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return fmt.Errorf("create private k3d directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), "k3d-*")
	if err != nil {
		return fmt.Errorf("create private k3d download: %w", err)
	}
	defer os.Remove(temporary.Name())
	digest := sha256.New()
	count, copyErr := io.Copy(io.MultiWriter(temporary, digest), io.LimitReader(response.Body, (256<<20)+1))
	closeErr := temporary.Close()
	if copyErr != nil {
		return fmt.Errorf("download private k3d: %w", copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("save private k3d: %w", closeErr)
	}
	if count > 256<<20 || hex.EncodeToString(digest.Sum(nil)) != checksum {
		return fmt.Errorf("private k3d download failed SHA256 verification")
	}
	if err := os.Chmod(temporary.Name(), 0700); err != nil {
		return fmt.Errorf("make private k3d executable: %w", err)
	}
	if err := os.Rename(temporary.Name(), destination); err != nil {
		return fmt.Errorf("install private k3d: %w", err)
	}
	return nil
}
