package platform

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	acmev1 "github.com/cert-manager/cert-manager/pkg/apis/acme/v1"
	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	"github.com/matryer/is"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestPocketIdConfiguration owns effective editable configuration and protects
// the public helper from exposing managed identity settings or mutating inputs.
func TestPocketIdConfiguration(t *testing.T) {
	is := is.New(t)
	values := map[string]any{"config": map[string]any{"appName": "Legacy", "smtpPassword": "legacy-secret", "smtpPort": 587}}
	environment := map[string]string{"APP_NAME": "Explicit", "SMTP_PASSWORD": "current-secret"}
	configuration, err := PocketIdConfiguration(values, environment)
	is.NoErr(err)
	is.Equal(configuration, map[string]string{"APP_NAME": "Explicit", "SMTP_PASSWORD": "current-secret", "SMTP_PORT": "587"})
	configuration["SMTP_PASSWORD"] = "changed"
	is.Equal(environment["SMTP_PASSWORD"], "current-secret")
	is.Equal(values["config"].(map[string]any)["smtpPassword"], "legacy-secret")
	_, err = PocketIdConfiguration(nil, map[string]string{"ENCRYPTION_KEY": "do-not-print"})
	is.True(err != nil && !strings.Contains(err.Error(), "do-not-print"))
	_, err = PocketIdConfiguration(map[string]any{"config": map[string]any{"staticApiKey": "do-not-print"}}, nil)
	is.True(err != nil && !strings.Contains(err.Error(), "do-not-print"))
	_, err = PocketIdConfiguration(map[string]any{"resources": map[string]any{"requests": map[string]any{"memory": "1Gi"}}}, nil)
	is.True(err != nil && strings.Contains(err.Error(), "limits cannot be smaller"))
	managed := PocketIdManagedEnvironment("https://id.example.test")
	is.Equal(managed["APP_URL"], "https://id.example.test")
	_, encryption := managed["ENCRYPTION_KEY"]
	_, api := managed["STATIC_API_KEY"]
	is.True(!encryption && !api)
	managed["HOST"] = "changed"
	is.Equal(PocketIdManagedEnvironment("https://id.example.test")["HOST"], "127.0.0.1")
}

// TestPocketIdCertificateIssuerSelection owns interpretation of observed issuer
// status and conservative defaults. It does not simulate certificate issuance.
func TestPocketIdCertificateIssuerSelection(t *testing.T) {
	production := certmanagerv1.ClusterIssuer{ObjectMeta: metav1.ObjectMeta{Name: "production", Generation: 2},
		Spec: certmanagerv1.IssuerSpec{IssuerConfig: certmanagerv1.IssuerConfig{ACME: &acmev1.ACMEIssuer{
			Server: "https://acme-v02.api.letsencrypt.org/directory",
			Solvers: []acmev1.ACMEChallengeSolver{{DNS01: &acmev1.ACMEChallengeSolverDNS01{
				Cloudflare: &acmev1.ACMEIssuerDNS01ProviderCloudflare{},
			}}},
		}}},
		Status: certmanagerv1.IssuerStatus{Conditions: []certmanagerv1.IssuerCondition{{Type: certmanagerv1.IssuerConditionReady, Status: cmmeta.ConditionTrue, ObservedGeneration: 2}}},
	}
	for name, test := range map[string]struct {
		change        func(*certmanagerv1.ClusterIssuer)
		ready         bool
		selectDefault bool
	}{
		"production": {func(*certmanagerv1.ClusterIssuer) {}, true, true},
		"empty selector": {func(issuer *certmanagerv1.ClusterIssuer) {
			issuer.Spec.ACME.Solvers[0].Selector = &acmev1.CertificateDNSNameSelector{}
		}, true, true},
		"staging": {func(issuer *certmanagerv1.ClusterIssuer) {
			issuer.Spec.ACME.Server = "https://acme-staging-v02.api.letsencrypt.org/directory"
		}, true, false},
		"custom ACME": {func(issuer *certmanagerv1.ClusterIssuer) {
			issuer.Spec.ACME.Server = "https://acme.example.test/directory"
		}, true, false},
		"unverified TLS": {func(issuer *certmanagerv1.ClusterIssuer) { issuer.Spec.ACME.SkipTLSVerify = true }, true, false},
		"selfsigned": {func(issuer *certmanagerv1.ClusterIssuer) {
			issuer.Spec.ACME = nil
			issuer.Spec.SelfSigned = &certmanagerv1.SelfSignedIssuer{}
		}, true, false},
		"http challenge": {func(issuer *certmanagerv1.ClusterIssuer) {
			issuer.Spec.ACME.Solvers[0].DNS01 = nil
			issuer.Spec.ACME.Solvers[0].HTTP01 = &acmev1.ACMEChallengeSolverHTTP01{}
		}, true, false},
		"other DNS provider": {func(issuer *certmanagerv1.ClusterIssuer) { issuer.Spec.ACME.Solvers[0].DNS01.Cloudflare = nil }, true, false},
		"DNS names": {func(issuer *certmanagerv1.ClusterIssuer) {
			issuer.Spec.ACME.Solvers[0].Selector = &acmev1.CertificateDNSNameSelector{DNSNames: []string{"id.example.test"}}
		}, true, false},
		"DNS zones": {func(issuer *certmanagerv1.ClusterIssuer) {
			issuer.Spec.ACME.Solvers[0].Selector = &acmev1.CertificateDNSNameSelector{DNSZones: []string{"example.test"}}
		}, true, false},
		"labels": {func(issuer *certmanagerv1.ClusterIssuer) {
			issuer.Spec.ACME.Solvers[0].Selector = &acmev1.CertificateDNSNameSelector{MatchLabels: map[string]string{"team": "identity"}}
		}, true, false},
		"ambiguous solvers": {func(issuer *certmanagerv1.ClusterIssuer) {
			issuer.Spec.ACME.Solvers = append(issuer.Spec.ACME.Solvers, issuer.Spec.ACME.Solvers[0])
		}, true, false},
		"stale readiness":   {func(issuer *certmanagerv1.ClusterIssuer) { issuer.Status.Conditions[0].ObservedGeneration = 1 }, false, false},
		"not ready":         {func(issuer *certmanagerv1.ClusterIssuer) { issuer.Status.Conditions[0].Status = cmmeta.ConditionFalse }, false, false},
		"unknown readiness": {func(issuer *certmanagerv1.ClusterIssuer) { issuer.Status.Conditions = nil }, false, false},
		"deleting":          {func(issuer *certmanagerv1.ClusterIssuer) { now := metav1.Now(); issuer.DeletionTimestamp = &now }, false, false},
	} {
		t.Run(name, func(t *testing.T) {
			is := is.New(t)
			issuer := production.DeepCopy()
			test.change(issuer)
			selection := pocketIdCertificateIssuers([]certmanagerv1.ClusterIssuer{*issuer})
			is.Equal(len(selection.Names) == 1, test.ready)
			is.Equal(selection.Default != "", test.selectDefault)
		})
	}
	is := is.New(t)
	other := production.DeepCopy()
	other.Name = "another"
	selection := pocketIdCertificateIssuers([]certmanagerv1.ClusterIssuer{production, *other})
	is.Equal(selection.Names, []string{"another", "production"})
	is.Equal(selection.Default, "")
}

// TestDiscoverPocketIdCertificateIssuers owns the read-only API boundary and
// missing-resource errors, without fabricating any controller reconciliation.
func TestDiscoverPocketIdCertificateIssuers(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusNotFound, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			is := is.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodGet || request.URL.Path != "/apis/cert-manager.io/v1/clusterissuers" {
					t.Errorf("unexpected issuer request: %s %s", request.Method, request.URL.Path)
					http.Error(writer, "unexpected request", http.StatusMethodNotAllowed)
					return
				}
				writer.Header().Set("Content-Type", "application/json")
				writer.WriteHeader(status)
				if status == http.StatusOK {
					_ = json.NewEncoder(writer).Encode(certmanagerv1.ClusterIssuerList{TypeMeta: metav1.TypeMeta{APIVersion: "cert-manager.io/v1", Kind: "ClusterIssuerList"}, Items: []certmanagerv1.ClusterIssuer{}})
					return
				}
				_ = json.NewEncoder(writer).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Failure", Code: int32(status)})
			}))
			defer server.Close()
			connection, _ := ownershipTestConnection(t, server.URL)
			selection, err := DiscoverPocketIdCertificateIssuers(t.Context(), connection)
			if status != http.StatusOK {
				is.True(err != nil)
				return
			}
			is.NoErr(err)
			is.Equal(selection.Names, []string{})
			server.Close()
			_, err = DiscoverPocketIdCertificateIssuers(t.Context(), connection)
			is.True(err != nil)
		})
	}
}
