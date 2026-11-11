package platform

import (
	"context"
	"fmt"
	"slices"
	"time"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	certmanagerclient "github.com/cert-manager/cert-manager/pkg/client/clientset/versioned/typed/certmanager/v1"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PocketIdCertificateIssuers describes currently ready management-cluster issuers.
type PocketIdCertificateIssuers struct {
	// Names lists ready, current-generation issuers in stable alphabetical order.
	Names []string
	// Default prefers the managed letsencrypt-cloudflare issuer, or one unambiguous known
	// production ACME issuer. It does not prove DNS coverage or credential access.
	Default string
}

// DiscoverPocketIdCertificateIssuers reads issuer status without changing cluster
// resources or requesting certificates. Unavailable clusters and missing CRDs
// return errors so callers can omit optional configuration hints.
func DiscoverPocketIdCertificateIssuers(ctx context.Context, connection kubernetes.Connection) (PocketIdCertificateIssuers, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	configuration := connection.RestConfig()
	// Kubernetes clients require a pointer to their transport configuration.
	client, err := certmanagerclient.NewForConfig(&configuration)
	if err != nil {
		return PocketIdCertificateIssuers{}, fmt.Errorf("connect to management certificate issuers: %w", err)
	}
	issuers, err := client.ClusterIssuers().List(ctx, metav1.ListOptions{})
	if err != nil {
		return PocketIdCertificateIssuers{}, fmt.Errorf("read management certificate issuers: %w", err)
	}
	return pocketIdCertificateIssuers(issuers.Items), nil
}

func pocketIdCertificateIssuers(issuers []certmanagerv1.ClusterIssuer) PocketIdCertificateIssuers {
	result := PocketIdCertificateIssuers{Names: []string{}}
	candidates := []string{}
	preferred := ""
	for _, issuer := range issuers {
		if issuer.Name == "" || issuer.DeletionTimestamp != nil {
			continue
		}
		ready := false
		for _, condition := range issuer.Status.Conditions {
			if condition.Type == certmanagerv1.IssuerConditionReady && condition.Status == cmmeta.ConditionTrue && condition.ObservedGeneration == issuer.Generation {
				ready = true
			}
		}
		if !ready {
			continue
		}
		result.Names = append(result.Names, issuer.Name)
		acme := issuer.Spec.ACME
		// An arbitrary ACME URL does not establish production trust. Restrict
		// automatic selection to Let's Encrypt's published production directory.
		if acme == nil || acme.Server != "https://acme-v02.api.letsencrypt.org/directory" || acme.SkipTLSVerify || len(acme.Solvers) != 1 {
			continue
		}
		solver := acme.Solvers[0]
		if solver.HTTP01 != nil || solver.DNS01 == nil {
			continue
		}
		if solver.DNS01.Cloudflare != nil {
			if selector := solver.Selector; selector != nil && (len(selector.MatchLabels) != 0 || len(selector.DNSNames) != 0 || len(selector.DNSZones) != 0) {
				if issuer.Name != CloudflareCertificateIssuer || checkNetworkOwner(&issuer) != nil || len(selector.MatchLabels) != 0 || len(selector.DNSNames) != 0 || len(selector.DNSZones) != 1 {
					continue
				}
			}
		} else {
			continue
		}
		candidates = append(candidates, issuer.Name)
		if issuer.Name == CloudflareCertificateIssuer && checkNetworkOwner(&issuer) == nil {
			preferred = issuer.Name
		}
	}
	slices.Sort(result.Names)
	if preferred != "" {
		result.Default = preferred
	} else if len(candidates) == 1 {
		result.Default = candidates[0]
	}
	return result
}
