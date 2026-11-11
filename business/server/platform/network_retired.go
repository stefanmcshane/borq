package platform

import (
	"context"
	"fmt"

	acmev1 "github.com/cert-manager/cert-manager/pkg/apis/acme/v1"
	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// Retire only the old installation's named resources. Other cert-manager
// webhooks, certificates and roots remain owned by their respective controllers.
// The Kubernetes SDK requires object pointers and UID/version precondition pointers.
func retireBorqDns01(ctx context.Context, client ctrlclient.Client, settings Settings) error {
	var issuers certmanagerv1.ClusterIssuerList
	if err := client.List(ctx, &issuers); err != nil {
		return fmt.Errorf("inspect retired DNS01 issuers: %w", err)
	}
	var certificates certmanagerv1.CertificateList
	if err := client.List(ctx, &certificates); err != nil {
		return fmt.Errorf("inspect retired DNS01 certificate references: %w", err)
	}
	for _, issuer := range issuers.Items {
		if issuer.Spec.ACME == nil {
			continue
		}
		for _, solver := range issuer.Spec.ACME.Solvers {
			if solver.DNS01 == nil || solver.DNS01.Webhook == nil || solver.DNS01.Webhook.GroupName != "dns01.borq.io" {
				continue
			}
			if (issuer.Name != "letsencrypt" && issuer.Name != "cloudflare") || checkNetworkOwner(&issuer) != nil {
				return fmt.Errorf("issuer %s still references the retired Borq DNS01 webhook; select native Cloudflare DNS01", issuer.Name)
			}
			used := false
			for _, certificate := range certificates.Items {
				used = used || certificate.Spec.IssuerRef.Kind == "ClusterIssuer" && certificate.Spec.IssuerRef.Name == issuer.Name
			}
			if used {
				// Preserve renewal for existing references, using the native solver.
				issuer.Spec.ACME.Solvers = managementCloudflareIssuer(*settings.Network.Cloudflare).Spec.ACME.Solvers
				if err := client.Update(ctx, &issuer, ctrlclient.FieldOwner("borq-network")); err != nil {
					return fmt.Errorf("replace retired issuer solver: %w", err)
				}
			} else if err := client.Delete(ctx, &issuer, ctrlclient.Preconditions{UID: &issuer.UID, ResourceVersion: &issuer.ResourceVersion}); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("retire unused issuer: %w", err)
			}
			break
		}
	}
	var challenges acmev1.ChallengeList
	if err := client.List(ctx, &challenges); err != nil {
		return fmt.Errorf("inspect pending DNS01 challenges: %w", err)
	}
	for _, challenge := range challenges.Items {
		if solver := challenge.Spec.Solver.DNS01; solver != nil && solver.Webhook != nil && solver.Webhook.GroupName == "dns01.borq.io" {
			return fmt.Errorf("certificate challenge %s/%s still uses the retired Borq webhook; retry that certificate request with the native issuer", challenge.Namespace, challenge.Name)
		}
	}
	for _, certificate := range certificates.Items {
		if certificate.Namespace != SystemNamespace || certificate.Spec.IssuerRef.Kind != "Issuer" {
			continue
		}
		if name := certificate.Spec.IssuerRef.Name; name != "borq-dns01-ca" && name != "borq-dns01-selfsigned" {
			continue
		}
		if certificate.Name != "borq-dns01-ca" && certificate.Name != "borq-dns01-serving" {
			return fmt.Errorf("certificate %s/%s still uses a retired Borq DNS01 CA", certificate.Namespace, certificate.Name)
		}
	}
	var owned []*unstructured.Unstructured
	for _, resource := range retiredDns01Resources(settings.Namespace) {
		object := &unstructured.Unstructured{}
		object.SetAPIVersion(resource.version)
		object.SetKind(resource.kind)
		err := client.Get(ctx, ctrlclient.ObjectKey{Namespace: resource.namespace, Name: resource.name}, object)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect retired DNS01 resource: %w", err)
		}
		if err := checkNetworkOwner(object); err != nil {
			return err
		}
		owned = append(owned, object)
	}
	for _, object := range owned {
		identity, version := object.GetUID(), object.GetResourceVersion()
		if err := client.Delete(ctx, object, ctrlclient.Preconditions{UID: &identity, ResourceVersion: &version}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("retire DNS01 %s %s/%s: %w", object.GetKind(), object.GetNamespace(), object.GetName(), err)
		}
	}
	return nil
}

type retiredDns01Resource struct{ version, kind, namespace, name string }

func retiredDns01Resources(namespace string) []retiredDns01Resource {
	return []retiredDns01Resource{
		{"apiregistration.k8s.io/v1", "APIService", "", "v1alpha1.dns01.borq.io"},
		{"apps/v1", "Deployment", SystemNamespace, "borq-dns01-webhook"},
		{"v1", "Service", SystemNamespace, "borq-dns01-webhook"},
		{"v1", "Service", SystemNamespace, "borq-management"},
		{"v1", "ServiceAccount", SystemNamespace, "borq-dns01-webhook"},
		{"rbac.authorization.k8s.io/v1", "ClusterRoleBinding", "", "borq-dns01-webhook-auth-delegator"},
		{"rbac.authorization.k8s.io/v1", "ClusterRoleBinding", "", "borq-dns01-webhook-solver"},
		{"rbac.authorization.k8s.io/v1", "ClusterRole", "", "borq-dns01-webhook-solver"},
		{"rbac.authorization.k8s.io/v1", "RoleBinding", "kube-system", "borq-dns01-webhook-auth-reader"},
		{"cert-manager.io/v1", "Certificate", SystemNamespace, "borq-dns01-serving"},
		{"cert-manager.io/v1", "Certificate", SystemNamespace, "borq-dns01-ca"},
		{"cert-manager.io/v1", "Issuer", SystemNamespace, "borq-dns01-ca"},
		{"cert-manager.io/v1", "Issuer", SystemNamespace, "borq-dns01-selfsigned"},
		{"v1", "Secret", SystemNamespace, "borq-dns01-serving"},
		{"v1", "Secret", SystemNamespace, "borq-dns01-ca"},
		{"v1", "Secret", SystemNamespace, "borq-dns01-webhook"},
		{"networking.k8s.io/v1", "Ingress", namespace, "borq-dns01"},
	}
}
