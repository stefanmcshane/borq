package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
)

// TestTargetOwnershipClaims creates ownership once and accepts retries only for
// the same organization and registration without updating the existing record.
func TestTargetOwnershipClaims(t *testing.T) {
	for _, input := range []struct {
		name     string
		existing map[string]string
		accepted bool
	}{
		{name: "new", accepted: true},
		{name: "same registration", existing: map[string]string{"clusterId": "cluster-a", "organization": "team", "name": "dev"}, accepted: true},
		{name: "other organization", existing: map[string]string{"clusterId": "cluster-b", "organization": "other", "name": "dev"}},
		{name: "other name", existing: map[string]string{"clusterId": "cluster-a", "organization": "team", "name": "renamed"}},
		{name: "incomplete owner", existing: map[string]string{"clusterId": "cluster-a"}},
	} {
		t.Run(input.name, func(t *testing.T) {
			assert := is.New(t)
			var calls []string
			var callsMutex sync.Mutex
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method == http.MethodGet && request.URL.Path == "/api/v1/namespaces/borq/configmaps/borq-platform-owner" {
					writeOwnershipResponse(writer, http.StatusNotFound, metav1.Status{Reason: metav1.StatusReasonNotFound, Code: 404})
					return
				}
				callsMutex.Lock()
				calls = append(calls, request.Method+" "+request.URL.Path)
				callsMutex.Unlock()
				switch {
				case request.Method == http.MethodPost && request.URL.Path == "/api/v1/namespaces/borq-system/configmaps":
					var submitted corev1.ConfigMap
					assert.NoErr(decodeOwnershipClaim(request, &submitted))
					assert.Equal(submitted.Name, "borq-platform-owner")
					assert.Equal(submitted.Data, map[string]string{"clusterId": "cluster-a", "organization": "team", "name": "dev"})
					assert.True(submitted.Immutable != nil && *submitted.Immutable)
					if input.existing != nil {
						writeOwnershipResponse(writer, http.StatusConflict, metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Reason: metav1.StatusReasonAlreadyExists, Code: 409})
						return
					}
					writeOwnershipResponse(writer, http.StatusCreated, submitted)
				case request.Method == http.MethodGet && request.URL.Path == "/api/v1/namespaces/borq-system/configmaps/borq-platform-owner":
					writeOwnershipResponse(writer, http.StatusOK, corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "borq-platform-owner", Namespace: "borq-system"}, Data: input.existing})
				default:
					t.Errorf("unexpected ownership request: %s %s", request.Method, request.URL.Path)
					http.Error(writer, "unexpected request", http.StatusBadRequest)
				}
			}))
			defer server.Close()
			connection, _ := ownershipTestConnection(t, server.URL)
			err := claimTargetOwner(context.Background(), connection, "borq-system", TargetInput{ID: "cluster-a", Organization: "team", Name: "dev"})
			assert.Equal(err == nil, input.accepted)
			if err != nil {
				assert.True(errors.Is(err, ErrTargetOwnership))
				assert.True(strings.Contains(err.Error(), "already owned"))
			}
			callsMutex.Lock()
			defer callsMutex.Unlock()
			if input.existing == nil {
				assert.Equal(calls, []string{"POST /api/v1/namespaces/borq-system/configmaps"})
			} else {
				assert.Equal(calls, []string{"POST /api/v1/namespaces/borq-system/configmaps", "GET /api/v1/namespaces/borq-system/configmaps/borq-platform-owner"})
			}
		})
	}
}

// TestConcurrentTargetOwnershipClaims proves competing registrations cannot both
// adopt a physical cluster when their first atomic create requests race.
func TestConcurrentTargetOwnershipClaims(t *testing.T) {
	assert := is.New(t)
	var mutex sync.Mutex
	var owner corev1.ConfigMap
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && request.URL.Path == "/api/v1/namespaces/borq/configmaps/borq-platform-owner" {
			writeOwnershipResponse(writer, http.StatusNotFound, metav1.Status{Reason: metav1.StatusReasonNotFound, Code: 404})
			return
		}
		mutex.Lock()
		defer mutex.Unlock()
		if request.Method == http.MethodPost && request.URL.Path == "/api/v1/namespaces/borq-system/configmaps" {
			if owner.Name != "" {
				writeOwnershipResponse(writer, http.StatusConflict, metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Reason: metav1.StatusReasonAlreadyExists, Code: 409})
				return
			}
			if err := decodeOwnershipClaim(request, &owner); err != nil {
				t.Errorf("decode ownership claim: %v", err)
			}
			writeOwnershipResponse(writer, http.StatusCreated, owner)
			return
		}
		if request.Method == http.MethodGet && request.URL.Path == "/api/v1/namespaces/borq-system/configmaps/borq-platform-owner" {
			writeOwnershipResponse(writer, http.StatusOK, owner)
			return
		}
		t.Errorf("unexpected ownership request: %s %s", request.Method, request.URL.Path)
		http.Error(writer, "unexpected request", http.StatusBadRequest)
	}))
	defer server.Close()
	connection, _ := ownershipTestConnection(t, server.URL)
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, id := range []string{"cluster-a", "cluster-b"} {
		go func() {
			<-start
			results <- claimTargetOwner(context.Background(), connection, "borq-system", TargetInput{ID: id, Organization: "team", Name: id})
		}()
	}
	close(start)
	accepted := 0
	for range 2 {
		if <-results == nil {
			accepted++
		}
	}
	assert.Equal(accepted, 1)
}

// TestTargetOwnershipRejectsBeforeCredentials verifies duplicate physical target
// registration cannot overwrite the scoped vault token or shared ESO bindings.
func TestTargetOwnershipRejectsBeforeCredentials(t *testing.T) {
	assert := is.New(t)
	var secretRequests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && request.URL.Path == "/api/v1/namespaces/borq/configmaps/borq-platform-owner" {
			writeOwnershipResponse(writer, http.StatusNotFound, metav1.Status{Reason: metav1.StatusReasonNotFound, Code: 404})
			return
		}
		if strings.Contains(request.URL.Path, "/secrets") || strings.Contains(request.URL.Path, "clustersecretstores") {
			secretRequests.Add(1)
		}
		switch {
		case request.URL.Path == "/api/v1/namespaces/borq-system" && (request.Method == http.MethodGet || request.Method == http.MethodPut):
			writeOwnershipResponse(writer, http.StatusOK, corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "borq-system", ResourceVersion: "1"}})
		case request.URL.Path == "/api/v1/namespaces/borq-system/configmaps" && request.Method == http.MethodPost:
			writeOwnershipResponse(writer, http.StatusConflict, metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Reason: metav1.StatusReasonAlreadyExists, Code: 409})
		case request.URL.Path == "/api/v1/namespaces/borq-system/configmaps/borq-platform-owner" && request.Method == http.MethodGet:
			writeOwnershipResponse(writer, http.StatusOK, corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "borq-platform-owner", Namespace: "borq-system"}, Data: map[string]string{"clusterId": "original", "organization": "first", "name": "dev"}})
		default:
			t.Errorf("unexpected target mutation: %s %s", request.Method, request.URL.Path)
			http.Error(writer, "unexpected request", http.StatusBadRequest)
		}
	}))
	defer server.Close()
	_, path := ownershipTestConnection(t, server.URL)
	_, err := ReconcileTarget(context.Background(), TargetInput{
		KubeconfigPath: path, Context: "selected", ID: "duplicate", Organization: "second", Name: "dev",
		SecretMount: "borq-duplicate", ClusterToken: "private-token",
		Settings: Settings{OpenBaoURL: "https://vault.test", TelemetryURL: "https://telemetry.test"},
	})
	assert.True(errors.Is(err, ErrTargetOwnership))
	assert.True(strings.Contains(err.Error(), "already owned"))
	assert.True(!strings.Contains(err.Error(), "private-token"))
	assert.Equal(secretRequests.Load(), int64(0))
}

func ownershipTestConnection(t *testing.T, address string) (kubernetes.Connection, string) {
	t.Helper()
	assert := is.New(t)
	path := filepath.Join(t.TempDir(), "kubeconfig")
	assert.NoErr(os.WriteFile(path, []byte(fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: wrong
  cluster:
    server: https://wrong.test:6443
- name: selected
  cluster:
    server: %q
contexts:
- name: wrong
  context:
    cluster: wrong
    user: test
- name: selected
  context:
    cluster: selected
    user: test
current-context: wrong
users:
- name: test
  user: {}
`, address)), 0o600))
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: path, Context: "selected"}, logger.Logger{})
	assert.NoErr(err)
	return connection, path
}

func writeOwnershipResponse(writer http.ResponseWriter, status int, body any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(body)
}

// TestCheckTargetOwnership is read-only and rejects foreign registrations before
// their credentials or database records can be accepted by the server.
func TestCheckTargetOwnership(t *testing.T) {
	for _, input := range []struct {
		name     string
		status   int
		owner    map[string]string
		accepted bool
		conflict bool
	}{
		{name: "no owner", status: http.StatusNotFound, accepted: true},
		{name: "same registration", status: http.StatusOK, owner: map[string]string{"clusterId": "cluster-a", "organization": "team", "name": "dev"}, accepted: true},
		{name: "other registration", status: http.StatusOK, owner: map[string]string{"clusterId": "cluster-b", "organization": "other", "name": "dev"}, conflict: true},
		{name: "access denied", status: http.StatusForbidden},
	} {
		t.Run(input.name, func(t *testing.T) {
			assert := is.New(t)
			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method == http.MethodGet && request.URL.Path == "/api/v1/namespaces/borq/configmaps/borq-platform-owner" {
					writeOwnershipResponse(writer, http.StatusNotFound, metav1.Status{Reason: metav1.StatusReasonNotFound, Code: 404})
					return
				}
				requests.Add(1)
				if request.Method != http.MethodGet || request.URL.Path != "/api/v1/namespaces/borq-system/configmaps/borq-platform-owner" {
					t.Errorf("ownership precheck attempted mutation: %s %s", request.Method, request.URL.Path)
				}
				if input.status != http.StatusOK {
					reason := metav1.StatusReasonNotFound
					if input.status == http.StatusForbidden {
						reason = metav1.StatusReasonForbidden
					}
					writeOwnershipResponse(writer, input.status, metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Reason: reason, Code: int32(input.status)})
					return
				}
				writeOwnershipResponse(writer, input.status, corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "borq-platform-owner", Namespace: "borq-system"}, Data: input.owner})
			}))
			defer server.Close()
			_, path := ownershipTestConnection(t, server.URL)
			err := CheckTargetOwnership(context.Background(), TargetInput{KubeconfigPath: path, Context: "selected", ID: "cluster-a", Organization: "team", Name: "dev"})
			assert.Equal(err == nil, input.accepted)
			assert.Equal(errors.Is(err, ErrTargetOwnership), input.conflict)
			assert.Equal(requests.Load(), int64(1))
		})
	}
}

// decodeOwnershipClaim accepts the Kubernetes SDK's protobuf request encoding;
// the object pointer is required by the Kubernetes decoder.
func decodeOwnershipClaim(request *http.Request, owner *corev1.ConfigMap) error {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return err
	}
	_, _, err = clientgoscheme.Codecs.UniversalDeserializer().Decode(body, nil, owner)
	return err
}

// TestLegacyTargetOwnership verifies both registration and atomic claim refuse
// old claims or unreadable ownership records without writing a new owner.
func TestLegacyTargetOwnership(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusForbidden} {
		for _, direct := range []bool{false, true} {
			t.Run(fmt.Sprintf("status=%d/direct=%t", status, direct), func(t *testing.T) {
				assert := is.New(t)
				server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					if request.Method != http.MethodGet || request.URL.Path != "/api/v1/namespaces/borq/configmaps/borq-platform-owner" {
						t.Errorf("legacy ownership permitted further access: %s %s", request.Method, request.URL.Path)
					}
					if status == http.StatusForbidden {
						writeOwnershipResponse(writer, status, metav1.Status{Reason: metav1.StatusReasonForbidden, Code: int32(status)})
						return
					}
					writeOwnershipResponse(writer, status, corev1.ConfigMap{Data: map[string]string{"clusterId": "cluster-a", "organization": "team", "name": "dev"}})
				}))
				defer server.Close()
				connection, path := ownershipTestConnection(t, server.URL)
				input := TargetInput{KubeconfigPath: path, Context: "selected", ID: "cluster-a", Organization: "team", Name: "dev"}
				var err error
				if direct {
					err = claimTargetOwner(context.Background(), connection, SystemNamespace, input)
				} else {
					err = CheckTargetOwnership(context.Background(), input)
				}
				assert.True(err != nil)
				if status == http.StatusOK {
					assert.True(errors.Is(err, ErrTargetOwnership))
					assert.True(strings.Contains(err.Error(), "migration is required"))
				} else {
					assert.True(strings.Contains(err.Error(), "read legacy target cluster ownership"))
				}
			})
		}
	}
}
