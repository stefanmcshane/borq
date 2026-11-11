package platform

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	acmev1 "github.com/cert-manager/cert-manager/pkg/apis/acme/v1"
	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	"github.com/matryer/is"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestApplicationRoutePolicy owns bounded issuer-domain interpretation, independent
// of controller readiness or access to provider credentials.
func TestApplicationRoutePolicy(t *testing.T) {
	for name, test := range map[string]struct {
		solvers []acmev1.ACMEChallengeSolver
		labels  map[string]string
		domains []string
	}{
		"explicit DNS zones normalize and deduplicate": {
			solvers: []acmev1.ACMEChallengeSolver{applicationRouteTestSolver("Example.Test", "a.example.test", "example.test", "*.example.test", ".example.test", "192.0.2.1", "example.test.", "bad_name.test", "")},
			domains: []string{"a.example.test", "example.test"},
		},
		"unbounded DNS solver grants no choices":  {solvers: []acmev1.ACMEChallengeSolver{{DNS01: &acmev1.ACMEChallengeSolverDNS01{}}}, domains: []string{}},
		"unbounded selector grants no choices":    {solvers: []acmev1.ACMEChallengeSolver{applicationRouteTestSolver()}, domains: []string{}},
		"exact names grant no suffix choice":      {solvers: []acmev1.ACMEChallengeSolver{{Selector: &acmev1.CertificateDNSNameSelector{DNSNames: []string{"*.example.test", "app.example.test"}, DNSZones: []string{"example.test"}}, DNS01: &acmev1.ACMEChallengeSolverDNS01{}}}, domains: []string{}},
		"HTTP zone grants choices":                {solvers: []acmev1.ACMEChallengeSolver{{Selector: &acmev1.CertificateDNSNameSelector{DNSZones: []string{"example.test"}}, HTTP01: &acmev1.ACMEChallengeSolverHTTP01{}}}, domains: []string{"example.test"}},
		"labels must match certificate":           {solvers: []acmev1.ACMEChallengeSolver{{Selector: &acmev1.CertificateDNSNameSelector{DNSZones: []string{"example.test"}, MatchLabels: map[string]string{"team": "web"}}, DNS01: &acmev1.ACMEChallengeSolverDNS01{}}}, labels: map[string]string{"team": "identity"}, domains: []string{}},
		"labels may match certificate":            {solvers: []acmev1.ACMEChallengeSolver{{Selector: &acmev1.CertificateDNSNameSelector{DNSZones: []string{"example.test"}, MatchLabels: map[string]string{"team": "web"}}, DNS01: &acmev1.ACMEChallengeSolverDNS01{}}}, labels: map[string]string{"team": "web"}, domains: []string{"example.test"}},
		"no challenge provider grants no choices": {solvers: []acmev1.ACMEChallengeSolver{{Selector: &acmev1.CertificateDNSNameSelector{DNSZones: []string{"example.test"}}}}, domains: []string{}},
	} {
		t.Run(name, func(t *testing.T) {
			assert := is.New(t)
			issuer := certmanagerv1.ClusterIssuer{ObjectMeta: metav1.ObjectMeta{Name: "assigned"}, Spec: certmanagerv1.IssuerSpec{IssuerConfig: certmanagerv1.IssuerConfig{ACME: &acmev1.ACMEIssuer{Solvers: test.solvers}}}}
			policy := applicationRoutePolicy(issuer, test.labels)
			assert.Equal(policy.CertificateIssuer, "assigned")
			assert.Equal(policy.Domains, test.domains)
		})
	}
	assert := is.New(t)
	policy := applicationRoutePolicy(certmanagerv1.ClusterIssuer{ObjectMeta: metav1.ObjectMeta{Name: "self-signed"}, Spec: certmanagerv1.IssuerSpec{IssuerConfig: certmanagerv1.IssuerConfig{SelfSigned: &certmanagerv1.SelfSignedIssuer{}}}}, nil)
	assert.Equal(policy.Domains, []string{})
	policy = applicationRoutePolicy(certmanagerv1.ClusterIssuer{Spec: certmanagerv1.IssuerSpec{IssuerConfig: certmanagerv1.IssuerConfig{CA: &certmanagerv1.CAIssuer{}}}}, nil)
	assert.Equal(policy.Domains, []string{})
}

// TestApplicationRouteHost owns hostname scope and solver precedence, including
// exact DNSNames which cert-manager never treats as wildcard suffix permissions.
func TestApplicationRouteHost(t *testing.T) {
	zoneSolver := applicationRouteTestSolver("example.test")
	exactSolver := applicationRouteTestSolver()
	exactSolver.Selector.DNSNames = []string{"reserved.example.test"}
	wildcardNameSolver := applicationRouteTestSolver()
	wildcardNameSolver.Selector.DNSNames = []string{"*.example.test"}
	labelSolver := applicationRouteTestSolver("app.example.test")
	labelSolver.Selector.MatchLabels = map[string]string{"team": "other"}
	for name, test := range map[string]struct {
		hostname string
		solvers  []acmev1.ACMEChallengeSolver
		domains  []string
		allowed  bool
	}{
		"one label":                          {"web.example.test", []acmev1.ACMEChallengeSolver{zoneSolver}, nil, true},
		"bare zone":                          {"example.test", []acmev1.ACMEChallengeSolver{zoneSolver}, nil, false},
		"two labels":                         {"web.nested.example.test", []acmev1.ACMEChallengeSolver{zoneSolver}, nil, false},
		"outside zone":                       {"web.other.test", []acmev1.ACMEChallengeSolver{zoneSolver}, nil, false},
		"suffix confusion":                   {"web.notexample.test", []acmev1.ACMEChallengeSolver{zoneSolver}, nil, false},
		"wildcard host":                      {"*.example.test", []acmev1.ACMEChallengeSolver{zoneSolver}, nil, false},
		"uppercase host":                     {"Web.example.test", []acmev1.ACMEChallengeSolver{zoneSolver}, nil, false},
		"invalid label":                      {"bad_name.example.test", []acmev1.ACMEChallengeSolver{zoneSolver}, nil, false},
		"empty label":                        {".example.test", []acmev1.ACMEChallengeSolver{zoneSolver}, nil, false},
		"exact override":                     {"reserved.example.test", []acmev1.ACMEChallengeSolver{zoneSolver, exactSolver}, nil, false},
		"unmatched exact name":               {"web.example.test", []acmev1.ACMEChallengeSolver{zoneSolver, exactSolver}, nil, true},
		"wildcard name stays exact":          {"web.example.test", []acmev1.ACMEChallengeSolver{wildcardNameSolver, zoneSolver}, nil, true},
		"nonmatching labels cannot override": {"app.example.test", []acmev1.ACMEChallengeSolver{zoneSolver, labelSolver}, nil, true},
		"HTTP explicit zone":                 {"web.example.test", []acmev1.ACMEChallengeSolver{{Selector: zoneSolver.Selector, HTTP01: &acmev1.ACMEChallengeSolverHTTP01{}}}, nil, true},
		"narrowed remote scope":              {"web.cluster.example.test", []acmev1.ACMEChallengeSolver{zoneSolver}, []string{"cluster.example.test"}, true},
		"remote scope blocks sibling":        {"web.example.test", []acmev1.ACMEChallengeSolver{zoneSolver}, []string{"cluster.example.test"}, false},
		"caller cannot expand issuer scope":  {"web.other.test", []acmev1.ACMEChallengeSolver{zoneSolver}, []string{"other.test"}, false},
		"selected specific zone permits its apex within parent policy": {"app.example.test", []acmev1.ACMEChallengeSolver{zoneSolver, applicationRouteTestSolver("app.example.test")}, []string{"example.test"}, true},
	} {
		t.Run(name, func(t *testing.T) {
			assert := is.New(t)
			issuer := certmanagerv1.ClusterIssuer{ObjectMeta: metav1.ObjectMeta{Name: "assigned"}, Spec: certmanagerv1.IssuerSpec{IssuerConfig: certmanagerv1.IssuerConfig{ACME: &acmev1.ACMEIssuer{Solvers: test.solvers}}}}
			policy := applicationRoutePolicy(issuer, nil)
			if test.domains != nil {
				policy.Domains = test.domains
			}
			err := ValidateApplicationRouteHost(policy, test.hostname)
			assert.Equal(err == nil, test.allowed)
		})
	}
}

// TestReadApplicationRoutePolicy owns the exact read-only Kubernetes boundary and
// sanitized failures; it does not claim certificate issuance or DNS access.
func TestReadApplicationRoutePolicy(t *testing.T) {
	for name, test := range map[string]struct {
		status   int
		deleting bool
	}{
		"read one issuer": {status: http.StatusOK},
		"missing issuer":  {status: http.StatusNotFound},
		"forbidden":       {status: http.StatusForbidden},
		"deleting issuer": {status: http.StatusOK, deleting: true},
	} {
		t.Run(name, func(t *testing.T) {
			assert := is.New(t)
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				requests++
				if request.Method != http.MethodGet || request.URL.Path != "/apis/cert-manager.io/v1/clusterissuers/assigned" {
					t.Errorf("unexpected request: %s %s", request.Method, request.URL.Path)
					http.Error(writer, "unexpected request", http.StatusMethodNotAllowed)
					return
				}
				writer.Header().Set("Content-Type", "application/json")
				writer.WriteHeader(test.status)
				if test.status != http.StatusOK {
					_ = json.NewEncoder(writer).Encode(metav1.Status{Status: "Failure", Code: int32(test.status), Message: "provider-secret-do-not-print"})
					return
				}
				solver := applicationRouteTestSolver("example.test")
				solver.DNS01.Cloudflare = &acmev1.ACMEIssuerDNS01ProviderCloudflare{Email: "private@example.test"}
				issuer := certmanagerv1.ClusterIssuer{TypeMeta: metav1.TypeMeta{APIVersion: "cert-manager.io/v1", Kind: "ClusterIssuer"}, ObjectMeta: metav1.ObjectMeta{Name: "assigned"}, Spec: certmanagerv1.IssuerSpec{IssuerConfig: certmanagerv1.IssuerConfig{ACME: &acmev1.ACMEIssuer{Solvers: []acmev1.ACMEChallengeSolver{solver}}}}}
				if test.deleting {
					now := metav1.Now()
					issuer.DeletionTimestamp = &now
				}
				_ = json.NewEncoder(writer).Encode(issuer)
			}))
			defer server.Close()
			connection, _ := ownershipTestConnection(t, server.URL)
			policy, err := ReadApplicationRoutePolicy(t.Context(), connection, "assigned", nil)
			assert.Equal(requests, 1)
			if test.status == http.StatusForbidden {
				assert.True(err != nil)
				assert.True(!strings.Contains(err.Error(), "provider-secret-do-not-print"))
				return
			}
			assert.NoErr(err)
			if test.status == http.StatusNotFound || test.deleting {
				assert.Equal(policy.CertificateIssuer, "assigned")
				assert.Equal(policy.Domains, []string{})
				assert.True(ValidateApplicationRouteHost(policy, "web.example.test") != nil)
				return
			}
			assert.NoErr(ValidateApplicationRouteHost(policy, "web.example.test"))
			encoded, err := json.Marshal(policy)
			assert.NoErr(err)
			assert.Equal(string(encoded), `{"certificateIssuer":"assigned","domains":["example.test"]}`)
			assert.Equal(policy.solvers[0].DNS01.Cloudflare, (*acmev1.ACMEIssuerDNS01ProviderCloudflare)(nil))
			_, err = ReadApplicationRoutePolicy(t.Context(), connection, "../not-an-issuer", nil)
			assert.True(err != nil)
			assert.Equal(requests, 1)
			server.Close()
			_, err = ReadApplicationRoutePolicy(t.Context(), connection, "assigned", nil)
			assert.True(err != nil && !strings.Contains(err.Error(), server.URL))
		})
	}
}

func applicationRouteTestSolver(zones ...string) acmev1.ACMEChallengeSolver {
	return acmev1.ACMEChallengeSolver{Selector: &acmev1.CertificateDNSNameSelector{DNSZones: zones}, DNS01: &acmev1.ACMEChallengeSolverDNS01{}}
}
