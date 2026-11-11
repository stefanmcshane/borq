package appapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/apps"
	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// TestPostgresCreationHonorsDisabledOrganization preserves an explicit opt-out
// at the HTTP boundary, including when database backups are disabled.
func TestPostgresCreationHonorsDisabledOrganization(t *testing.T) {
	assert := is.New(t)
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "borq.sqlite")
	database, err := sqlite.New(ctx, databasePath)
	assert.NoErr(err)
	assert.NoErr(database.Commit(ctx, []sqlite.Write{
		{Collection: "organization", Key: "team", Value: json.RawMessage(`{"name":"team","managedAddons":false}`)},
		{Collection: "space", Key: "team/dev", Value: json.RawMessage(`{"space":{"id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","organization":"team","name":"dev","cluster":"target","namespace":"team-dev"}}`)},
	}))
	assert.NoErr(database.Close())
	manager, err := apps.NewManager(ctx, apps.NewInput{DatabasePath: databasePath})
	assert.NoErr(err)
	defer manager.Close()
	api, err := NewHandler(manager)
	assert.NoErr(err)
	for _, body := range []string{`{}`, `{"backup":{"enabled":false}}`} {
		response := httptest.NewRecorder()
		api.ServeHTTP(response, httptest.NewRequest(http.MethodPut, "/api/v1/organizations/team/spaces/dev/managed-addons/postgres/database", strings.NewReader(body)))
		assert.Equal(response.Code, http.StatusBadRequest)
		var problem Problem
		assert.NoErr(json.Unmarshal(response.Body.Bytes(), &problem))
		assert.Equal(problem.Message, "Managed PostgreSQL is disabled for this organization. Set managedAddons to true in management configuration to enable it.")
	}
	organizations, err := manager.ListOrganizations(ctx)
	assert.NoErr(err)
	assert.Equal(len(organizations), 1)
	assert.True(!organizations[0].ManagedAddons)
	databases, err := manager.ListPostgres(ctx, "team", "dev")
	assert.NoErr(err)
	assert.Equal(len(databases), 0)
}

// TestManagementDocumentSchema protects the public named hierarchy and shared space fragment at the HTTP decoding boundary.
func TestManagementDocumentSchema(t *testing.T) {
	assert := is.New(t)
	specification, err := GetSwagger()
	assert.NoErr(err)
	server := handler{schemas: specification.Components.Schemas}
	for _, input := range []struct {
		schema, body string
		valid        bool
	}{
		{"ManagementConfiguration", `{"components":{"cloudnative-pg":{"repo":"https://cloudnative-pg.github.io/charts","chart":"cloudnative-pg","version":"0.29.1","values":{"replicaCount":1}}},"s3":{"region":"us-east-1","bucket":"shared","pathStyle":true,"accessKey":"private-key","secretKey":"private-secret"},"organizations":[{"name":"team","managedAddons":true,"clusters":[{"name":"local","kubeconfig":"private-kubeconfig"}],"spaces":[{"name":"dev","cluster":"local","apps":[{"name":"api","replicas":0}],"customAddons":[{"name":"cache","repository":"https://charts.example.com","chart":"redis","version":"1.2.3"}],"managedAddons":{"postgres":[{"name":"database","backup":{"enabled":false}}]}}]}]}`, true},
		{"ManagementConfiguration", `{"borqPublicUrl":"https://borq.example.test","organizations":[{"name":"team","managedPocketId":{"issuerUrl":"https://id.example.test","adminEmail":"owner@example.test"}}]}`, true},
		{"ManagementConfiguration", `{"managedAddons":{"pocketId":[]}}`, false},
		{"ManagementConfiguration", `{"organizations":[{"name":"team","managedPocketId":{"name":"identity","organization":"team","issuerUrl":"https://id.example.test","adminEmail":"owner@example.test"}}]}`, false},
		{"PocketIdInput", `{"issuerUrl":"https://id.example.test","adminUsername":"owner","adminEmail":"owner@example.test","storage":"2Gi"}`, true},
		{"PocketIdInput", `{"password":"private-secret"}`, false},
		{"SpaceConfiguration", `{"managedAddons":{"pocketid":[{"name":"identity","issuerUrl":"https://id.example.test","adminEmail":"owner@example.test"}]}}`, true},
		{"SpaceConfiguration", `{"managedAddons":{"pocketid":[{"name":"Uppercase"}]}}`, false},
		{"PostgresInput", `{"backup":{"enabled":true,"schedule":"0 0 * * *","restoreCheck":{"schedule":"0 3 * * 0"}}}`, true},
		{"ManagementConfiguration", `{"network":{"cloudflare":{"wildcardDomain":"example.com","zoneId":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","acmeEmail":"admin@example.com","tunnel":{"enabled":true}}}}`, true},
		{"ManagementConfiguration", `{"network":{"cloudflare":{"wildcardDomain":"example.com","zoneId":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","acmeEmail":"admin@example.com","tunnel":{"enabled":true,"token":"private-tunnel-token"}}}}`, false},
		{"ManagementConfiguration", `{"network":{"cloudflare":{"wildcardDomain":"example.com","zoneId":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","acmeEmail":"admin@example.com","tunnel":{}}}}`, false},
		{"PostgresInput", `{"snapshots":"disabled"}`, false},
		{"PostgresInput", `{"restoreCheck":{"enabled":false}}`, false},
		{"PostgresInput", `{"backup":{"restoreCheck":{"enabled":false}}}`, false},
		{"SpaceConfiguration", `{"apps":[],"customAddons":[],"managedAddons":{"postgres":[{"name":"database","storage":"2Gi"}]}}`, true},
		{"ManagementConfiguration", `{"registries":{"ghcr.io":{"username":"github-user","token":"private-token"}}}`, true},
		{"SpaceConfiguration", `{"registries":{"ghcr.io":{"credentialsRef":"borq/registry-auth/space/ref"}}}`, true},
		{"SpaceConfiguration", `{"registries":{"hub":{"server":"docker.io","username":"user","token":"token"}}}`, true},
		{"SpaceConfiguration", `{"registries":{"hub":{"server":123,"username":"user","token":"token"}}}`, false},
		{"SpaceConfiguration", `{"registries":{"ghcr.io":{"password":"unsupported"}}}`, false},
		{"ManagementConfiguration", `{"backups":{"enabled":false,"encryptionKey":"age1public"}}`, true},
		{"ManagementConfiguration", `{"backups":{"enabled":false,"recipient":"age1public"}}`, true},
		{"ManagementConfiguration", `{"backups":{"enabled":false,"encryptionKey":123}}`, false},
		{"ManagementConfiguration", `{"version":1}`, false},
		{"ManagementConfiguration", `{"profile":"development"}`, false},
		{"SpaceConfiguration", `{"managedAddons":{"postgres":[{"name":"database","unrecognized":true}]}}`, false},
		{"ManagementConfiguration", `{"s3":{"region":"one","region":"two","bucket":"shared","pathStyle":true}}`, false},
	} {
		writer := httptest.NewRecorder()
		var output any
		accepted := server.decodeBody(writer, httptest.NewRequest(http.MethodPut, "/", strings.NewReader(input.body)), input.schema, &output)
		if accepted != input.valid {
			t.Fatalf("%s accepted=%v want=%v: %s", input.schema, accepted, input.valid, writer.Body.String())
		}
		if !accepted {
			assert.True(!strings.Contains(writer.Body.String(), "private-secret"))
		}
	}
}

// TestManagedRoutesRejectInvalidRequestsBeforeManager protects path binding, strict bodies, bounded logs and secret cache headers.
func TestManagedRoutesRejectInvalidRequestsBeforeManager(t *testing.T) {
	assert := is.New(t)
	api, err := NewHandler(apps.Manager{})
	assert.NoErr(err)
	base := "/api/v1/organizations/team/spaces/dev/managed-addons/postgres/"
	for _, input := range []struct {
		method, path, body string
		status             int
		message            string
	}{
		{http.MethodPut, "/api/v1/organizations/team/spaces/dev/managed-addons/pocketid/identity", `{"password":"private-password"}`, 400, "Pocket ID settings must use supported configuration fields. Credentials are generated automatically."},
		{http.MethodPut, "/api/v1/organizations/team/spaces/dev/managed-addons/pocketid/Uppercase", `{}`, 400, "Pocket ID names must contain 1 to 47 lowercase letters, digits or hyphens, and start and end with a letter or digit."},
		{http.MethodPost, "/api/v1/organizations/team/spaces/dev/managed-addons/pocketid/identity/enrollment", `{"password":"private-password"}`, 400, "this operation does not accept a request body"},
		{http.MethodPut, base + "database", `{"password":"private-password"}`, 400, "PostgreSQL settings must use supported configuration fields. Passwords are generated automatically."},
		{http.MethodPut, base + "test", `{"password":"private-password"}`, 400, "PostgreSQL settings must use supported configuration fields. Passwords are generated automatically."},
		{http.MethodPut, base + "test-1", `{"password":"private-password"}`, 400, "PostgreSQL settings must use supported configuration fields. Passwords are generated automatically."},
		{http.MethodPut, base + "database", `{"instances":17}`, 400, "PostgreSQL instances must be an integer between 1 and 16."},
		{http.MethodPut, base + "database", `{"private-password":"private-password"}`, 400, "PostgreSQL settings must use supported configuration fields. Passwords are generated automatically."},
		{http.MethodPost, base + "database/restore", `{"backupId":"approved","specification":{"backup":{"enabled":"private-password"}}}`, 400, "PostgreSQL backup enabled must be a boolean."},
		{http.MethodPost, base + "database/restore", `{"backupId":"approved","recoveryTarget":"private-password"}`, 400, "PostgreSQL recoveryTarget must be an RFC3339 date-time string."},
		{http.MethodPut, base + "database?apply=invalid", `{}`, 400, "invalid request path"},
		{http.MethodPut, base + "Test1", `{}`, 400, "PostgreSQL names must contain 1 to 47 lowercase letters, digits or hyphens, and start and end with a letter or digit."},
		{http.MethodPut, "/api/v1/organizations/team/spaces/dev/configuration", `{"managedAddons":{"postgres":[{"name":"Uppercase"}]}}`, 400, "PostgreSQL names must contain 1 to 47 lowercase letters, digits or hyphens, and start and end with a letter or digit."},
		{http.MethodGet, base + "database/logs?limit=0", "", 400, "log limit must be between 1 and 1000"},
		{http.MethodGet, base + "database/logs?limit=1001", "", 400, "log limit must be between 1 and 1000"},
		{http.MethodPost, "/api/v1/management/destroy", `{}`, 400, "this operation does not accept a request body"},
		{http.MethodPost, base + "database/restore-check", `{"jobUid":"job"}`, 401, "a projected restore-check token is required"},
		{http.MethodGet, base + "database/restore-check/operation?jobUid=job", "", 401, "a projected restore-check token is required"},
	} {
		t.Run(input.method+" "+input.path, func(t *testing.T) {
			assert := is.New(t)
			response := httptest.NewRecorder()
			api.ServeHTTP(response, httptest.NewRequest(input.method, input.path, strings.NewReader(input.body)))
			assert.Equal(response.Code, input.status)
			cacheControl := "no-store"
			if strings.HasSuffix(input.path, "/enrollment") {
				cacheControl = "no-store, private"
			}
			assert.Equal(response.Header().Get("Cache-Control"), cacheControl)
			assert.True(!strings.Contains(response.Body.String(), "private-password"))
			var problem Problem
			assert.NoErr(json.Unmarshal(response.Body.Bytes(), &problem))
			assert.Equal(problem.Message, input.message)
		})
	}
}

// TestManagementStorageResponseUsesSafeModel prevents inline credentials from surviving response conversion.
func TestManagementStorageResponseUsesSafeModel(t *testing.T) {
	assert := is.New(t)
	writer := httptest.NewRecorder()
	respond[ManagementS3](writer, http.StatusOK, map[string]any{"region": "us-east-1", "bucket": "shared", "pathStyle": true, "credentialsRef": "vault:shared/1", "accessKey": "private-access", "secretKey": "private-secret", "sessionToken": "private-session"}, nil)
	assert.Equal(writer.Code, http.StatusOK)
	var output map[string]any
	assert.NoErr(json.Unmarshal(writer.Body.Bytes(), &output))
	assert.Equal(output["credentialsRef"], "vault:shared/1")
	for _, key := range []string{"accessKey", "secretKey", "sessionToken"} {
		_, exists := output[key]
		assert.True(!exists)
	}
}

// TestManagementStorageAcceptsOperation protects the HTTP status, generated
// client response, and polling route for asynchronous S3 test-and-save requests.
func TestManagementStorageAcceptsOperation(t *testing.T) {
	assert := is.New(t)
	ctx := t.Context()
	directory := t.TempDir()
	manager, err := apps.NewManager(ctx, apps.NewInput{
		DatabasePath: filepath.Join(directory, "borq.sqlite"), ManagementKubeconfigPath: filepath.Join(directory, "unconfigured-kubeconfig"),
	})
	assert.NoErr(err)
	t.Cleanup(func() { assert.NoErr(manager.Close()) })
	api, err := NewHandler(manager)
	assert.NoErr(err)
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)
	client, err := NewClientWithResponses(server.URL)
	assert.NoErr(err)
	endpoint, reference := "https://storage.example.com", "borq/platform/rustfs"
	accepted, err := client.PutManagementS3WithResponse(ctx, ManagementS3Input{
		Endpoint: &endpoint, Region: "us-east-1", Bucket: "management-data", PathStyle: true, CredentialsRef: &reference,
	})
	assert.NoErr(err)
	assert.Equal(accepted.StatusCode(), http.StatusAccepted)
	assert.True(accepted.JSON202 != nil)
	assert.True(accepted.JSON202.ID != "")
	assert.Equal(accepted.JSON202.Status, Queued)
	specification, err := GetSwagger()
	assert.NoErr(err)
	var payload any
	assert.NoErr(json.Unmarshal(accepted.Body, &payload))
	assert.NoErr(specification.Components.Schemas["Operation"].Value.VisitJSON(payload))
	status, err := client.GetManagementOperationWithResponse(ctx, accepted.JSON202.ID)
	assert.NoErr(err)
	assert.Equal(status.StatusCode(), http.StatusOK)
	assert.True(status.JSON200 != nil)
	assert.Equal(status.JSON200.ID, accepted.JSON202.ID)
	assert.NoErr(json.Unmarshal(status.Body, &payload))
	assert.NoErr(specification.Components.Schemas["Operation"].Value.VisitJSON(payload))
}

// TestManagementApplyRetryAndConflicts protects upgrade compatibility for an
// accepted pre-fingerprint bootstrap and actionable, secret-free HTTP conflicts.
func TestManagementApplyRetryAndConflicts(t *testing.T) {
	assert := is.New(t)
	ctx := t.Context()
	databasePath := filepath.Join(t.TempDir(), "state.sqlite")
	database, err := sqlite.New(ctx, databasePath)
	assert.NoErr(err)
	assert.NoErr(database.Commit(ctx, []sqlite.Write{
		{Collection: "management-operation", Key: "previous-bootstrap", Value: json.RawMessage(`{"id":"previous-bootstrap","status":"running","phase":"space"}`)},
		{Collection: "management-task", Key: "previous-bootstrap", Value: json.RawMessage(`{"configuration":{"organizations":[{"name":"borq","clusters":[{"id":"derived-cluster-id","organization":"borq","name":"production","inCluster":true,"reconcile":true}],"spaces":[{"name":"production","cluster":"production"}]}]},"next":2}`)},
	}))
	assert.NoErr(database.Close())
	// Management startup defers workers until infrastructure is ready; no cluster
	// controller result is needed to exercise acceptance of an existing operation.
	manager, err := apps.NewManager(ctx, apps.NewInput{DatabasePath: databasePath, ManagementEnabled: true, InfrastructureEnabled: true,
		ManagementKubeconfigPath: "testdata/deferred-management.kubeconfig",
		PlatformSettings:         platform.Settings{ClusterName: "production", OpenBaoURL: "http://vault.test:8200", TelemetryURL: "http://telemetry.test:7834", Network: platform.NetworkSettings{Tailscale: &platform.TailscaleSettings{}}}})
	assert.NoErr(err)
	defer manager.Close()
	api, err := NewHandler(manager)
	assert.NoErr(err)
	for _, test := range []struct {
		name, body, message string
		status              int
	}{
		{"same bootstrap", `{"organizations":[{"name":"borq","clusters":[{"name":"production","inCluster":true,"reconcile":true}],"spaces":[{"name":"production","cluster":"production"}]}]}`, "", http.StatusAccepted},
		{"different request", `{"organizations":[{"name":"different"}]}`, "management operation previous-bootstrap is still active; its stored configuration cannot be matched to this request; wait for it to finish before reapplying", http.StatusConflict},
		{"inline secret", `{"s3":{"region":"us-east-1","bucket":"backups","pathStyle":true,"accessKey":"private-access","secretKey":"private-secret"}}`, "management operation previous-bootstrap is still active; its stored configuration cannot be matched to this request; wait for it to finish before reapplying", http.StatusConflict},
		{"network change", `{"network":{"tailscale":{"managementHostname":"different"}}}`, "networking differs from bootstrap; rerun management bootstrap with these network settings", http.StatusConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			response := httptest.NewRecorder()
			api.ServeHTTP(response, httptest.NewRequest(http.MethodPut, "/api/v1/management/configuration", strings.NewReader(test.body)))
			assert.Equal(response.Code, test.status)
			assert.True(!strings.Contains(response.Body.String(), "private-"))
			if test.status == http.StatusAccepted {
				var operation Operation
				assert.NoErr(json.Unmarshal(response.Body.Bytes(), &operation))
				assert.Equal(operation.ID, "previous-bootstrap")
				assert.Equal(operation.Status, Running)
				return
			}
			var problem Problem
			assert.NoErr(json.Unmarshal(response.Body.Bytes(), &problem))
			assert.Equal(problem.Message, test.message)
		})
	}
}
