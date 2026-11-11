package platform

import (
	"testing"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	"github.com/matryer/is"
)

// The management path must use the native solver and retain its restricted DNS
// selector when old webhook issuers coexist during a test installation repair.
func TestNetworkManagementCloudflareIssuer(t *testing.T) {
	assert := is.New(t)
	issuer := managementCloudflareIssuer(CloudflareSettings{WildcardDomain: "prod-test.borq.io", AcmeEmail: "admin@borq.io"})
	assert.NoErr(checkNetworkOwner(issuer))
	assert.Equal(issuer.Name, "letsencrypt-cloudflare")
	assert.Equal(issuer.Namespace, "")
	acme := issuer.Spec.ACME
	assert.Equal(acme.Server, "https://acme-v02.api.letsencrypt.org/directory")
	assert.Equal(acme.Email, "admin@borq.io")
	assert.Equal(acme.PrivateKey.Name, "borq-letsencrypt-account")
	assert.Equal(len(acme.Solvers), 1)
	solver := acme.Solvers[0]
	assert.Equal(solver.Selector.DNSZones, []string{"prod-test.borq.io"})
	assert.True(solver.HTTP01 == nil && solver.DNS01.Webhook == nil)
	assert.Equal(solver.DNS01.Cloudflare.APIToken.Name, "borq-cloudflare-api-token")
	assert.Equal(solver.DNS01.Cloudflare.APIToken.Key, "api-token")
	issuer.Status.Conditions = []certmanagerv1.IssuerCondition{{Type: certmanagerv1.IssuerConditionReady, Status: cmmeta.ConditionTrue}}
	legacy := issuer.DeepCopy()
	legacy.Name = "letsencrypt"
	assert.Equal(pocketIdCertificateIssuers([]certmanagerv1.ClusterIssuer{*legacy, *issuer}).Default, "letsencrypt-cloudflare")
	issuer.Labels = nil
	assert.Equal(pocketIdCertificateIssuers([]certmanagerv1.ClusterIssuer{*issuer}).Default, "")
}
