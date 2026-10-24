package cluster

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/matryer/is"
)

// TestPinnedK3dAssets supports native Apple Silicon and Linux without shell installers.
func TestPinnedK3dAssets(t *testing.T) {
	is := is.New(t)
	apple, err := k3dAssetForPlatform("darwin", "arm64")
	is.NoErr(err)
	is.Equal(apple.name, "k3d-darwin-arm64")
	is.Equal(apple.checksum, "8da468daa7dc7cf7cdd4735f90a9bb05179fa27858250f62e3d8cdf5b5ca0698")
	linux, err := k3dAssetForPlatform("linux", "amd64")
	is.NoErr(err)
	is.Equal(linux.checksum, "dbaa79a76ace7f4ca230a1ff41dc7d8a5036a8ad0309e9c54f9bf3836dbe853e")
	_, err = k3dAssetForPlatform("unsupported", "unsupported")
	is.True(err != nil)
}

// TestDownloadK3dVerifiesBeforeInstalling makes a failed or corrupted download leave the current tool intact.
func TestDownloadK3dVerifiesBeforeInstalling(t *testing.T) {
	is := is.New(t)
	payload := []byte("test executable")
	digest := sha256.Sum256(payload)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(payload)
	}))
	defer server.Close()
	destination := filepath.Join(t.TempDir(), "bin", "k3d")
	is.NoErr(downloadK3d(context.Background(), *server.Client(), server.URL, destination, hex.EncodeToString(digest[:])))
	info, err := os.Stat(destination)
	is.NoErr(err)
	is.Equal(info.Mode().Perm(), os.FileMode(0700))
	err = downloadK3d(context.Background(), *server.Client(), server.URL, destination, "incorrect-checksum")
	is.True(err != nil)
	current, err := os.ReadFile(destination)
	is.NoErr(err)
	is.Equal(string(current), string(payload))
	entries, err := os.ReadDir(filepath.Dir(destination))
	is.NoErr(err)
	is.Equal(len(entries), 1)
	err = downloadK3d(context.Background(), *server.Client(), server.URL+"/missing", destination, "incorrect-checksum")
	is.True(err != nil)
}
