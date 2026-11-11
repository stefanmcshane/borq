package platform

import (
	"context"
	"strings"
	"testing"

	acmev1 "github.com/cert-manager/cert-manager/pkg/apis/acme/v1"
	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// These fixtures cover ownership and reference guards only; they do not claim
// that a controller issued or renewed any certificate.
func TestNetworkRetiredDns01Ownership(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(map[bool]string{false: "owned", true: "foreign"}[foreign], func(t *testing.T) {
			assert := is.New(t)
			secret := &corev1.Secret{ObjectMeta: networkMetadata(SystemNamespace, "borq-dns01-ca")}
			service := &corev1.Service{ObjectMeta: networkMetadata(SystemNamespace, "borq-dns01-webhook")}
			if foreign {
				service.Labels = nil
			}
			client := fake.NewClientBuilder().WithScheme(kubernetes.NewScheme()).WithObjects(secret, service).Build()
			err := retireBorqDns01(context.Background(), client, Settings{Namespace: ManagementNamespace, Network: networkTestSettings()})
			if foreign {
				assert.True(err != nil && strings.Contains(err.Error(), "not owned"))
				assert.NoErr(client.Get(t.Context(), ctrlclient.ObjectKeyFromObject(secret), &corev1.Secret{}))
				return
			}
			assert.NoErr(err)
			assert.True(apierrors.IsNotFound(client.Get(t.Context(), ctrlclient.ObjectKeyFromObject(secret), &corev1.Secret{})))
			assert.True(apierrors.IsNotFound(client.Get(t.Context(), ctrlclient.ObjectKeyFromObject(service), &corev1.Service{})))
		})
	}
}

func TestNetworkRetiredDns01IssuerReferences(t *testing.T) {
	for _, alias := range []string{"letsencrypt", "cloudflare"} {
		for _, used := range []bool{false, true} {
			t.Run(alias+"/"+map[bool]string{false: "unused alias removed", true: "used alias becomes native"}[used], func(t *testing.T) {
				assert := is.New(t)
				issuer := managementCloudflareIssuer(CloudflareSettings{WildcardDomain: "example.com", AcmeEmail: "admin@example.com"})
				issuer.Name = alias
				issuer.Spec.ACME.Solvers[0].DNS01 = &acmev1.ACMEChallengeSolverDNS01{Webhook: &acmev1.ACMEIssuerDNS01ProviderWebhook{GroupName: "dns01.borq.io", SolverName: "borq"}}
				objects := []ctrlclient.Object{issuer}
				if used {
					objects = append(objects, &certmanagerv1.Certificate{ObjectMeta: metav1.ObjectMeta{Namespace: ManagementNamespace, Name: "existing-public"}, Spec: certmanagerv1.CertificateSpec{IssuerRef: cmmeta.ObjectReference{Kind: "ClusterIssuer", Name: issuer.Name}}})
				}
				client := fake.NewClientBuilder().WithScheme(kubernetes.NewScheme()).WithObjects(objects...).Build()
				assert.NoErr(retireBorqDns01(t.Context(), client, Settings{Namespace: ManagementNamespace, Network: networkTestSettings()}))
				var current certmanagerv1.ClusterIssuer
				err := client.Get(t.Context(), ctrlclient.ObjectKey{Name: issuer.Name}, &current)
				if used {
					assert.NoErr(err)
					assert.True(current.Spec.ACME.Solvers[0].DNS01.Cloudflare != nil && current.Spec.ACME.Solvers[0].DNS01.Webhook == nil)
				} else {
					assert.True(apierrors.IsNotFound(err))
				}
			})
		}
	}
}
