package apps

import (
	"strings"
	"testing"

	"github.com/matryer/is"
	"k8s.io/apimachinery/pkg/util/validation"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// TestManagedIngressNames preserves uniqueness for maximum-length route names
// without truncating names into collisions or exceeding Kubernetes limits.
func TestManagedIngressNames(t *testing.T) {
	assert := is.New(t)
	appName := strings.Repeat("a", 50)
	firstRoute := strings.Repeat("r", 62) + "a"
	secondRoute := strings.Repeat("r", 62) + "b"
	image, port := "frontend:v1", 8080
	routes := []Ingress{{Name: firstRoute, Host: "one.example.com"}, {Name: secondRoute, Host: "two.example.com"}}
	configuration, err := Merge(Configuration{}, Patch{Apps: &[]AppPatch{{Name: appName, Image: &image, Port: &port, Ingresses: &routes}}})
	assert.NoErr(err)
	rendered, err := Render(configuration, "shop", "production")
	assert.NoErr(err)
	secrets := map[string]bool{}
	for _, route := range configuration.Apps[0].Ingresses {
		ingress := decodeResource[gatewayv1.Gateway](t, findManifest(t, rendered, "Gateway", appName+"-"+route.Name))
		secretName := string(ingress.Spec.Listeners[0].TLS.CertificateRefs[0].Name)
		assert.Equal(secretName, appName+"-"+route.Name+"-tls")
		assert.Equal(validation.IsDNS1123Subdomain(secretName), []string(nil))
		assert.True(len(secretName) <= 253)
		assert.True(!secrets[secretName])
		secrets[secretName] = true
	}
	inspected, err := Inspect(rendered.Manifests)
	assert.NoErr(err)
	assert.Equal(inspected, configuration)
}

// TestCertificateIssuerValidation rejects malformed issuer references before
// rendering and accepts the full Kubernetes ClusterIssuer name boundary.
func TestCertificateIssuerValidation(t *testing.T) {
	assert := is.New(t)
	longestName := strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 61)
	name, err := normalizeCertificateIssuer(longestName)
	assert.NoErr(err)
	assert.Equal(name, longestName)
	for _, name := range []string{"Invalid_Issuer", "issuer/name", " issuer", longestName + "a"} {
		_, err := RenderForPlan(Configuration{}, "shop", "production", "plan-1", name)
		assert.True(err != nil)
		assert.True(strings.Contains(err.Error(), "invalid certificate issuer"))
	}
}
