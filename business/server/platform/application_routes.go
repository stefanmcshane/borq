package platform

import (
	"context"
	"errors"
	"net"
	"slices"
	"strings"
	"time"

	acmev1 "github.com/cert-manager/cert-manager/pkg/apis/acme/v1"
	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	certmanagerclient "github.com/cert-manager/cert-manager/pkg/client/clientset/versioned/typed/certmanager/v1"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// ApplicationRoutePolicy bounds application hostnames to an assigned issuer's explicit zones.
type ApplicationRoutePolicy struct {
	// CertificateIssuer is the exact ClusterIssuer assigned to the application space.
	CertificateIssuer string `json:"certificateIssuer"`
	// Domains lists candidate suffixes; ValidateApplicationRouteHost also checks solver overrides.
	Domains           []string `json:"domains"`
	solvers           []acmev1.ACMEChallengeSolver
	certificateLabels map[string]string
}

// ReadApplicationRoutePolicy reads one assigned ClusterIssuer without accessing credentials or changing resources.
// Issuer readiness is deliberately separate from hostname policy and certificate issuance.
func ReadApplicationRoutePolicy(ctx context.Context, connection kubernetes.Connection, issuerName string, certificateLabels map[string]string) (ApplicationRoutePolicy, error) {
	if issuerName == "" || len(validation.IsDNS1123Subdomain(issuerName)) != 0 {
		return ApplicationRoutePolicy{}, errors.New("application space has no valid certificate issuer")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	configuration := connection.RestConfig()
	// Kubernetes clients require a pointer to their transport configuration.
	client, err := certmanagerclient.NewForConfig(&configuration)
	if err != nil {
		return ApplicationRoutePolicy{}, errors.New("cannot connect to the application's certificate issuer")
	}
	issuer, err := client.ClusterIssuers().Get(ctx, issuerName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return ApplicationRoutePolicy{CertificateIssuer: issuerName, Domains: []string{}}, nil
	}
	if err != nil {
		return ApplicationRoutePolicy{}, errors.New("cannot read the application's certificate issuer")
	}
	if issuer.DeletionTimestamp != nil {
		return ApplicationRoutePolicy{CertificateIssuer: issuerName, Domains: []string{}}, nil
	}
	return applicationRoutePolicy(*issuer, certificateLabels), nil
}

func applicationRoutePolicy(issuer certmanagerv1.ClusterIssuer, certificateLabels map[string]string) ApplicationRoutePolicy {
	policy := ApplicationRoutePolicy{CertificateIssuer: issuer.Name, Domains: []string{}, certificateLabels: map[string]string{}}
	for name, value := range certificateLabels {
		policy.certificateLabels[name] = value
	}
	if issuer.Spec.ACME == nil {
		return policy
	}
	for _, solver := range issuer.Spec.ACME.Solvers {
		// Retain selection metadata only; provider configuration must not escape this read boundary.
		candidate := acmev1.ACMEChallengeSolver{Selector: solver.Selector.DeepCopy()}
		if solver.DNS01 != nil {
			candidate.DNS01 = &acmev1.ACMEChallengeSolverDNS01{}
		}
		if solver.HTTP01 != nil {
			candidate.HTTP01 = &acmev1.ACMEChallengeSolverHTTP01{}
		}
		policy.solvers = append(policy.solvers, candidate)
		if candidate.Selector == nil || len(candidate.Selector.DNSNames) != 0 || (candidate.DNS01 == nil && candidate.HTTP01 == nil) {
			continue
		}
		if !applicationRouteLabelsMatch(candidate.Selector.MatchLabels, policy.certificateLabels) {
			continue
		}
		for _, zone := range candidate.Selector.DNSZones {
			if domain := applicationRouteDomain(zone); domain != "" {
				policy.Domains = append(policy.Domains, domain)
			}
		}
	}
	slices.Sort(policy.Domains)
	policy.Domains = slices.Compact(policy.Domains)
	return policy
}

// ValidateApplicationRouteHost requires one DNS label beneath an explicit issuer zone,
// including cert-manager's exact-name, zone, label and ordering selection rules.
// It validates configuration scope, not DNS ownership or successful certificate issuance.
func ValidateApplicationRouteHost(policy ApplicationRoutePolicy, hostname string) error {
	if applicationRouteDomain(hostname) != hostname || !applicationRouteHostInDomains(hostname, policy.Domains) {
		return errors.New("public route hostname must be one subdomain beneath an assigned certificate issuer domain")
	}
	solver, found := applicationRouteSolver(policy, hostname)
	if !found || solver.Selector == nil || len(solver.Selector.DNSNames) != 0 {
		return errors.New("public route hostname is not covered by an assigned certificate issuer domain solver")
	}
	for _, zone := range solver.Selector.DNSZones {
		if domain := applicationRouteDomain(zone); domain != "" && (hostname == domain || strings.HasSuffix(hostname, "."+domain)) {
			return nil
		}
	}
	return errors.New("public route hostname is not covered by an assigned certificate issuer domain solver")
}

func applicationRouteSolver(policy ApplicationRoutePolicy, hostname string) (acmev1.ACMEChallengeSolver, bool) {
	selected := -1
	selectedScore := [3]int{}
	for index, solver := range policy.solvers {
		if solver.DNS01 == nil && solver.HTTP01 == nil {
			continue
		}
		score := [3]int{}
		if selector := solver.Selector; selector != nil {
			if !applicationRouteLabelsMatch(selector.MatchLabels, policy.certificateLabels) {
				continue
			}
			score[2] = len(selector.MatchLabels)
			if len(selector.DNSNames) != 0 {
				if !slices.Contains(selector.DNSNames, hostname) {
					continue
				}
				score[0] = 1
			}
			for _, zone := range selector.DNSZones {
				zone = strings.TrimSuffix(strings.ToLower(zone), ".")
				if zone != "" && (hostname == zone || strings.HasSuffix(hostname, "."+zone)) {
					score[1] = max(score[1], strings.Count(zone, ".")+1)
				}
			}
			if len(selector.DNSZones) != 0 && score[1] == 0 {
				continue
			}
		}
		// cert-manager prioritizes exact DNSNames, then zone specificity, then
		// matching labels; equal scores retain the first solver in issuer order.
		if selected < 0 || slices.Compare(score[:], selectedScore[:]) > 0 {
			selected, selectedScore = index, score
		}
	}
	if selected < 0 {
		return acmev1.ACMEChallengeSolver{}, false
	}
	return policy.solvers[selected], true
}

func applicationRouteLabelsMatch(required, actual map[string]string) bool {
	for name, value := range required {
		if observed, exists := actual[name]; !exists || observed != value {
			return false
		}
	}
	return true
}

func applicationRouteHostInDomains(hostname string, domains []string) bool {
	for _, domain := range domains {
		prefix, matched := strings.CutSuffix(hostname, "."+domain)
		if matched && prefix != "" && !strings.Contains(prefix, ".") && len(validation.IsDNS1123Label(prefix)) == 0 {
			return true
		}
	}
	return false
}

func applicationRouteDomain(domain string) string {
	domain = strings.ToLower(domain)
	if domain == "" || net.ParseIP(domain) != nil || len(validation.IsDNS1123Subdomain(domain)) != 0 {
		return ""
	}
	return domain
}
