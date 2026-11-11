package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matryer/is"
)

// TestNetworkSettingsFile keeps credentials and unknown fields out of the mounted public settings file.
func TestNetworkSettingsFile(t *testing.T) {
	assert := is.New(t)
	path := filepath.Join(t.TempDir(), "network.json")
	for _, value := range []string{`{"tailscale":{},"apiToken":"private-value"}`, `{} {}`, `{}`, `null`, strings.Repeat(" ", 64*1024+1)} {
		assert.NoErr(os.WriteFile(path, []byte(value), 0600))
		_, err := readNetworkSettings(path, "management")
		assert.True(err != nil)
		assert.True(!strings.Contains(err.Error(), "private-value"))
	}
	assert.NoErr(os.WriteFile(path, []byte(`{"tailscale":{}}`), 0600))
	settings, err := readNetworkSettings(path, "management")
	assert.NoErr(err)
	assert.True(settings.Tailscale != nil && settings.Cloudflare == nil)
}

func TestNetworkExposureYaml(t *testing.T) {
	for _, mode := range []string{"tunnel", "direct"} {
		t.Run(mode, func(t *testing.T) {
			assert := is.New(t)
			path := filepath.Join(t.TempDir(), "management.yaml")
			body := "network:\n  cloudflare:\n    exposure: " + mode + "\n    wildcardDomain: '*.prod-test.borq.io'\n    zoneId: '11111111111111111111111111111111'\n    acmeEmail: admin@example.com\n"
			if mode == "direct" {
				body += "    directTarget: public-lb.example.net\n"
			}
			assert.NoErr(os.WriteFile(path, []byte(body), 0600))
			configuration, err := readManagementConfiguration(path, "")
			assert.NoErr(err)
			assert.True(configuration.Network != nil && configuration.Network.Cloudflare != nil)
			assert.Equal(string(*configuration.Network.Cloudflare.Exposure), mode)
			if mode == "direct" {
				assert.Equal(*configuration.Network.Cloudflare.DirectTarget, "public-lb.example.net")
			}
			assert.NoErr(os.WriteFile(path, []byte(strings.Replace(body, "exposure: "+mode, "exposure: wrong", 1)), 0600))
			_, err = readManagementConfiguration(path, "")
			assert.True(err != nil && strings.Contains(err.Error(), "configuration does not match"))
		})
	}
}
