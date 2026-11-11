package platform

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/openbao"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestRustfsVaultLossFence owns pre-provisioning evidence: an empty vault cannot overwrite surviving credentials or archive storage.
func TestRustfsVaultLossFence(t *testing.T) {
	for _, test := range []struct {
		name                    string
		projected, vault        map[string]string
		volume, foreign, legacy bool
		problem                 string
	}{
		{name: "fresh installation"},
		{name: "retained root", projected: map[string]string{"RUSTFS_ACCESS_KEY": "access", "RUSTFS_SECRET_KEY": "secret", "RUSTFS_KMS_ROOT_PROBE": "original"}, vault: map[string]string{"RUSTFS_ACCESS_KEY": "access", "RUSTFS_SECRET_KEY": "secret", "RUSTFS_KMS_ROOT_PROBE": "original"}},
		{name: "whole vault lost", projected: map[string]string{"RUSTFS_ACCESS_KEY": "access", "RUSTFS_SECRET_KEY": "secret", "RUSTFS_KMS_ROOT_PROBE": "original"}, problem: "credentials no longer match"},
		{name: "unrelated vault", projected: map[string]string{"RUSTFS_ACCESS_KEY": "access", "RUSTFS_SECRET_KEY": "secret", "RUSTFS_KMS_ROOT_PROBE": "original"}, vault: map[string]string{"RUSTFS_ACCESS_KEY": "access", "RUSTFS_SECRET_KEY": "secret", "RUSTFS_KMS_ROOT_PROBE": "replacement"}, problem: "root no longer matches"},
		{name: "encrypted state without probe", projected: map[string]string{"RUSTFS_ACCESS_KEY": "access", "RUSTFS_SECRET_KEY": "secret", "RUSTFS_KMS_VAULT_TOKEN": "existing"}, vault: map[string]string{"RUSTFS_ACCESS_KEY": "access", "RUSTFS_SECRET_KEY": "secret"}, problem: "encryption state exists"},
		{name: "deleted projection with retained volume", volume: true, problem: "storage exists without encryption continuity"},
		{name: "foreign volume", volume: true, foreign: true, problem: "outside the managed release"},
		{name: "original vault recovers projection", volume: true, vault: map[string]string{"RUSTFS_KMS_ROOT_PROBE": "original"}},
		{name: "legacy ESO projection", legacy: true, projected: map[string]string{"RUSTFS_ACCESS_KEY": "access", "RUSTFS_SECRET_KEY": "secret"}, vault: map[string]string{"RUSTFS_ACCESS_KEY": "access", "RUSTFS_SECRET_KEY": "secret"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			check := is.New(t)
			vaultServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				check.Equal(request.Method, http.MethodGet)
				check.Equal(request.URL.Path, "/v1/secret/data/borq/platform/rustfs")
				if test.vault == nil {
					writer.WriteHeader(http.StatusNotFound)
					return
				}
				writeOwnershipResponse(writer, http.StatusOK, map[string]any{"data": map[string]any{"data": map[string]any{"values": test.vault}, "metadata": map[string]any{"version": 1}}})
			}))
			defer vaultServer.Close()
			vault, err := openbao.NewClient(openbao.NewClientInput{Address: vaultServer.URL})
			check.NoErr(err)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				check.Equal(request.Method, http.MethodGet)
				switch request.URL.Path {
				case "/api":
					writeOwnershipResponse(writer, http.StatusOK, metav1.APIVersions{Versions: []string{"v1"}})
				case "/apis":
					writeOwnershipResponse(writer, http.StatusOK, metav1.APIGroupList{})
				case "/api/v1":
					writeOwnershipResponse(writer, http.StatusOK, metav1.APIResourceList{GroupVersion: "v1", APIResources: []metav1.APIResource{{Name: "secrets", Kind: "Secret", Namespaced: true}, {Name: "persistentvolumeclaims", Kind: "PersistentVolumeClaim", Namespaced: true}}})
				case "/api/v1/namespaces/management/secrets/borq-rustfs":
					if test.projected == nil {
						writeOwnershipResponse(writer, http.StatusNotFound, metav1.Status{Reason: metav1.StatusReasonNotFound, Code: 404})
						return
					}
					secret := corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "borq-rustfs", Namespace: "management", Labels: managedLabels()}, Data: map[string][]byte{}}
					if test.legacy {
						secret.Labels = nil
						secret.OwnerReferences = []metav1.OwnerReference{{APIVersion: "external-secrets.io/v1", Kind: "ExternalSecret", Name: "borq-rustfs"}}
					}
					for name, value := range test.projected {
						secret.Data[name] = []byte(value)
					}
					writeOwnershipResponse(writer, http.StatusOK, secret)
				case "/api/v1/namespaces/management/persistentvolumeclaims/rustfs-data":
					if !test.volume {
						writeOwnershipResponse(writer, http.StatusNotFound, metav1.Status{Reason: metav1.StatusReasonNotFound, Code: 404})
						return
					}
					claim := corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "rustfs-data", Namespace: "management", Annotations: map[string]string{"meta.helm.sh/release-name": "rustfs", "meta.helm.sh/release-namespace": "management"}}}
					if test.foreign {
						claim.Annotations["meta.helm.sh/release-name"] = "someone-else"
					}
					writeOwnershipResponse(writer, http.StatusOK, claim)
				default:
					t.Errorf("unexpected continuity request %s", request.URL.Path)
					writer.WriteHeader(500)
				}
			}))
			defer server.Close()
			connection, _ := ownershipTestConnection(t, server.URL)
			err = CheckRustfsEncryptionContinuity(t.Context(), connection, Settings{Namespace: "management"}, vault)
			if test.problem == "" {
				check.NoErr(err)
			} else {
				check.True(err != nil)
				check.True(strings.Contains(err.Error(), test.problem))
			}
		})
	}
}
