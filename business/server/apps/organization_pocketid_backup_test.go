package apps

import (
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/openbao"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// TestPocketIdBackupCredentialPersistence owns vault custody across manager restarts.
// Real scoped S3 permissions and archive restoration belong to the platform cluster test.
func TestPocketIdBackupCredentialPersistence(t *testing.T) {
	assert := is.New(t)
	fixture := newManagementStorageFixture(t)
	upstream, err := url.Parse(fixture.s3.URL)
	assert.NoErr(err)
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	var accounts atomic.Int32
	storage := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case strings.HasPrefix(request.URL.Path, "/rustfs/admin/v3/info-service-account"):
			writer.WriteHeader(http.StatusNotFound)
		case request.URL.Path == "/rustfs/admin/v3/add-service-account":
			accounts.Add(1)
			writer.WriteHeader(http.StatusOK)
		case request.Method == http.MethodHead:
			writer.WriteHeader(http.StatusOK)
		default:
			proxy.ServeHTTP(writer, request)
		}
	}))
	defer storage.Close()
	configuration := NewInput{DatabasePath: filepath.Join(t.TempDir(), "borq.sqlite"), ManagementEnabled: true, InfrastructureEnabled: true, ManagementKubeconfigPath: fixture.kubeconfig,
		PlatformSettings: platform.Settings{Namespace: "borq", Profile: "dev", S3Endpoint: storage.URL, OpenBaoURL: "http://vault.test", TelemetryURL: "http://borq.test"}}
	manager, err := NewManager(t.Context(), configuration)
	assert.NoErr(err)
	defer func() { _ = manager.Close() }()
	organization, err := manager.CreateOrganization(t.Context(), "backup")
	assert.NoErr(err)
	_, err = manager.SetupOrganizationPocketId(t.Context(), "backup", OrganizationPocketIdInput{IssuerURL: "https://id.test", BaseURL: "https://borq.test", CertificateIssuer: "issuer", AdminUsername: "owner", AdminEmail: "owner@example.test"})
	assert.NoErr(err)
	stored, _, err := manager.readPocketId(t.Context(), organization.ID)
	assert.NoErr(err)
	vault, err := platform.ManagementVault(t.Context(), configuration.ManagementKubeconfigPath, "borq")
	assert.NoErr(err)
	_, err = vault.WriteKv(t.Context(), "secret", "borq/platform/rustfs", openbao.WriteKvInput{Values: map[string]string{"RUSTFS_ACCESS_KEY": "root-key", "RUSTFS_SECRET_KEY": "root-secret"}})
	assert.NoErr(err)
	keys, err := manager.pocketIdBackupCredentials(t.Context(), stored)
	assert.NoErr(err)
	assert.True(keys.AccessKey != "root-key" && keys.SecretKey != "root-secret")
	assert.Equal(accounts.Load(), int32(1))
	assert.NoErr(manager.Close())
	manager, err = NewManager(t.Context(), configuration)
	assert.NoErr(err)
	reopened, record, err := manager.readPocketId(t.Context(), organization.ID)
	assert.NoErr(err)
	assert.True(!strings.Contains(string(record.Value), keys.SecretKey))
	assert.True(!strings.Contains(string(record.Value), "root-secret"))
	// Removing the root credential proves retries reuse their saved restricted account.
	assert.NoErr(vault.DeleteKvMetadata(t.Context(), "secret", "borq/platform/rustfs"))
	reused, err := manager.pocketIdBackupCredentials(t.Context(), reopened)
	assert.NoErr(err)
	assert.Equal(reused, keys)
	assert.Equal(accounts.Load(), int32(1))
	fixture.mutex.Lock()
	fixture.denied = true
	fixture.mutex.Unlock()
	_, err = manager.pocketIdBackupCredentials(t.Context(), reopened)
	assert.True(err != nil)
	assert.True(strings.Contains(err.Error(), "Check management storage"))
	assert.True(!strings.Contains(err.Error(), keys.SecretKey))

	// Exercise the recovery guard for a recorded installation. Keeping the root
	// credential available makes an accidental replacement possible and observable.
	fixture.mutex.Lock()
	fixture.denied = false
	fixture.mutex.Unlock()
	_, err = vault.WriteKv(t.Context(), "secret", "borq/platform/rustfs", openbao.WriteKvInput{Values: map[string]string{"RUSTFS_ACCESS_KEY": "root-key", "RUSTFS_SECRET_KEY": "root-secret"}})
	assert.NoErr(err)
	reopened.BackupConfigured = true
	write, err := documentWrite("organization-pocketid", organization.ID, reopened, record.Revision)
	assert.NoErr(err)
	assert.NoErr(manager.database.Commit(t.Context(), []sqlite.Write{write}))
	_, before, err := manager.readPocketId(t.Context(), organization.ID)
	assert.NoErr(err)
	assert.NoErr(vault.DeleteKvMetadata(t.Context(), "secret", reopened.credentialsPath()+"/backup"))
	_, err = manager.pocketIdBackupCredentials(t.Context(), reopened)
	assert.True(err != nil)
	assert.True(strings.Contains(err.Error(), "backup credentials are missing"))
	assert.Equal(accounts.Load(), int32(1))
	_, after, err := manager.readPocketId(t.Context(), organization.ID)
	assert.NoErr(err)
	assert.Equal(after, before)
}

// TestPocketIdBackupDestination keeps management identity archives independent
// of organization storage and external primary-S3 overrides.
func TestPocketIdBackupDestination(t *testing.T) {
	assert := is.New(t)
	stored := organizationPocketIdRecord{ID: strings.Repeat("a", 32)}
	for _, test := range []struct {
		settings platform.Settings
		endpoint string
	}{
		{platform.Settings{}, "http://rustfs-svc.borq-mgmt.svc:9000"},
		{platform.Settings{Namespace: "custom-management", S3: platform.S3Settings{Endpoint: "https://external.test", Bucket: "unrelated"}}, "http://rustfs-svc.custom-management.svc:9000"},
		{platform.Settings{ApiUrl: "https://management.test"}, "http://management.test:30900"},
		{platform.Settings{S3Endpoint: "https://rustfs.test", ApiUrl: "https://management.test"}, "https://rustfs.test"},
	} {
		manager, err := NewManager(t.Context(), NewInput{DatabasePath: filepath.Join(t.TempDir(), "state.sqlite"), PlatformSettings: test.settings})
		assert.NoErr(err)
		storage := manager.pocketIdBackupStorage(stored)
		assert.Equal(storage.Endpoint, test.endpoint)
		assert.Equal(storage.Bucket, "borq-pocketid-"+stored.ID)
		assert.Equal(storage.Prefix, "sqlite")
		assert.True(storage.PathStyle)
		assert.NoErr(manager.Close())
	}
}
