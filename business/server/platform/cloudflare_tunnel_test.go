package platform

import (
	"encoding/json"
	"github.com/matryer/is"
	"strings"
	"testing"
)

// TestCloudflareTunnelConfiguration verifies opt-in defaults are copied rather
// than mutating stored configuration, while existing direct ingress remains valid.
func TestCloudflareTunnelConfiguration(t *testing.T) {
	is := is.New(t)
	network := networkTestSettings()
	network.Cloudflare.Tunnel = &CloudflareTunnelSettings{Enabled: true}
	normalized, err := NormalizeNetworkSettings(network, "production")
	is.NoErr(err)
	is.True(strings.HasPrefix(normalized.Cloudflare.Tunnel.Image, "cloudflare/cloudflared:2026.10.7@sha256:"))
	is.Equal(network.Cloudflare.Tunnel.Image, "")
	normalized.Cloudflare.Tunnel.Enabled = false
	is.True(network.Cloudflare.Tunnel.Enabled)
	_, err = NormalizeNetworkSettings(networkTestSettings(), "production")
	is.NoErr(err)
	network.Cloudflare.Tunnel.Image = "bad image"
	_, err = NormalizeNetworkSettings(network, "production")
	is.True(err != nil)
}

// TestCloudflaredCredentialBoundary verifies pods receive only a tunnel-token
// file, keep API credentials out of args and environment, and roll on token rotation.
func TestCloudflaredCredentialBoundary(t *testing.T) {
	is := is.New(t)
	network := networkTestSettings()
	network.Cloudflare.Tunnel = &CloudflareTunnelSettings{Enabled: true}
	network, err := NormalizeNetworkSettings(network, "production")
	is.NoErr(err)
	settings := Settings{Namespace: ManagementNamespace, Network: network}
	deployment := managementTunnelDeployment(settings, "private-run-token")
	is.Equal(*deployment.Spec.Replicas, int32(2))
	pod := deployment.Spec.Template.Spec
	is.True(!*pod.AutomountServiceAccountToken)
	is.True(*pod.SecurityContext.RunAsNonRoot)
	container := pod.Containers[0]
	is.Equal(container.Args, []string{"tunnel", "--no-autoupdate", "--metrics", "0.0.0.0:2000", "run", "--token-file", "/etc/cloudflared/token"})
	is.Equal(len(container.Env), 0)
	is.Equal(container.ReadinessProbe.HTTPGet.Path, "/ready")
	is.Equal(pod.Volumes[0].Secret.SecretName, managementTunnelName)
	is.True(container.VolumeMounts[0].ReadOnly)
	encoded, err := json.Marshal(deployment)
	is.NoErr(err)
	is.True(!strings.Contains(string(encoded), "private-run-token"))
	rotated := managementTunnelDeployment(settings, "rotated-run-token")
	is.True(deployment.Spec.Template.Annotations["borq.run/tunnel-token-sha256"] != rotated.Spec.Template.Annotations["borq.run/tunnel-token-sha256"])
}
