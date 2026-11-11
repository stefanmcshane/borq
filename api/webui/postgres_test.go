package webui

import (
	"encoding/json"
	"fmt"
	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/api/appapi"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestManagedPostgresViews keeps database sections scoped, restore choices eligible and credentials outside rendered pages.
func TestManagedPostgresViews(t *testing.T) {
	reason := "incomplete metadata"
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name, query, tab, subtab, content, panel string
		status                                   int
	}{
		{name: "overview", tab: "overview", content: "Recovery verification"},
		{name: "backups", query: "&tab=backups", tab: "backups", content: "Restore from a backup"},
		{name: "configuration", query: "&tab=configuration", tab: "configuration", content: "Database settings"},
		{name: "metrics", query: "&tab=observability", tab: "observability", subtab: "metrics", panel: "metrics", content: "Reading CPU"},
		{name: "logs", query: "&tab=observability&subtab=logs&range=12h&search=error&end=2026-10-24T12%3A00%3A00Z", tab: "observability", subtab: "logs", panel: "logs", content: "Database logs"},
		{name: "unknown section", query: "&tab=other", status: http.StatusNotFound},
		{name: "unknown observability", query: "&tab=observability&subtab=other", status: http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			backupsRead := false
			web, err := NewHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				var output any
				switch {
				case request.URL.Path == "/api/v1/organizations":
					output = []appapi.Organization{{Name: "team"}}
				case strings.HasSuffix(request.URL.Path, "/spaces"):
					output = []appapi.Space{{ID: "dev", Name: "dev", Cluster: "local"}}
				case strings.HasSuffix(request.URL.Path, "/clusters"):
					output = []appapi.Cluster{}
				case strings.HasSuffix(request.URL.Path, "/postgres"):
					output = []appapi.Postgres{{Name: "database"}}
				case strings.HasSuffix(request.URL.Path, "/postgres/database"):
					output = appapi.Postgres{Name: "database", EnvironmentGroup: "postgres-database-immutable-id", State: "ready", Operation: "change", Protection: appapi.PostgresProtection{Status: "ready", Message: "Backups verified"}, RestoreCheck: appapi.PostgresRestoreCheck{Status: "succeeded"}}
				case strings.HasSuffix(request.URL.Path, "/postgres-operations/change"):
					output = appapi.Operation{ID: "change", SpaceID: "dev", Status: "running", Message: "Updating database"}
				case strings.HasSuffix(request.URL.Path, "/backups"):
					backupsRead = true
					output = []appapi.PostgresBackup{{ID: "approved", SourceName: "database", CompletedAt: now, Eligible: true}, {ID: "rejected", CompletedAt: now, Reason: &reason}}
				default:
					t.Errorf("unexpected API request %s", request.URL.Path)
					http.NotFound(writer, request)
					return
				}
				_ = json.NewEncoder(writer).Encode(output)
			}))
			assert.NoErr(err)
			response := httptest.NewRecorder()
			web.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/ui/page?view=postgres-detail&organization=team&space=dev&postgres=database"+test.query, nil))
			if test.status != 0 {
				assert.Equal(response.Code, test.status)
				assert.True(!backupsRead)
				return
			}

			page := decodeView[pageData](t, response, "workspace")
			data := page.Postgres
			assert.Equal(data.Tab, test.tab)
			assert.Equal(data.Subtab, test.subtab)
			assert.Equal(data.Database.Name, "database")
			assert.Equal(data.Database.EnvironmentGroup, "postgres-database-immutable-id")
			assert.Equal(data.Operation.Message, "Updating database")
			assert.True(!strings.Contains(response.Body.String(), "PGPASSWORD"))
			assert.Equal(backupsRead, test.tab == "backups")
			if test.tab == "backups" {
				assert.Equal(len(data.Backups), 2)
				assert.Equal(data.Backups[0].ID, "approved")
				assert.True(data.Backups[0].Eligible)
				assert.Equal(data.Backups[1].ID, "rejected")
				assert.True(!data.Backups[1].Eligible)
				assert.Equal(*data.Backups[1].Reason, "incomplete metadata")
			}
			if test.subtab == "logs" {
				path, err := url.Parse(data.Path)
				assert.NoErr(err)
				assert.Equal(path.Path, "/api/ui/postgres")
				assert.Equal(path.Query().Get("organization"), "team")
				assert.Equal(path.Query().Get("space"), "dev")
				assert.Equal(path.Query().Get("postgres"), "database")
				assert.Equal(path.Query().Get("range"), "12h")
				assert.Equal(path.Query().Get("search"), "error")
				assert.Equal(path.Query().Get("end"), "2026-10-24T12:00:00Z")
			}
			if test.tab == "overview" {
				assert.Equal(data.Database.Protection.Status, "ready")
				assert.Equal(data.Database.RestoreCheck.Status, "succeeded")
			}

		})
	}
}

// TestManagedPostgresDeletionProgress keeps accepted deletion visible after its database record disappears and rejects another space's operation.
func TestManagedPostgresDeletionProgress(t *testing.T) {
	for _, operationSpace := range []string{"dev", "other"} {
		t.Run(operationSpace, func(t *testing.T) {
			assert := is.New(t)
			web, err := NewHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				var output any
				switch request.URL.Path {
				case "/api/v1/organizations":
					output = []appapi.Organization{{Name: "team"}}
				case "/api/v1/organizations/team/spaces":
					output = []appapi.Space{{ID: "dev", Name: "dev", Cluster: "local"}}
				case "/api/v1/organizations/team/clusters":
					output = []appapi.Cluster{}
				case "/api/v1/organizations/team/spaces/dev/managed-addons/postgres":
					output = []appapi.Postgres{}
				case "/api/v1/postgres-operations/deletion":
					output = appapi.Operation{ID: "deletion", SpaceID: operationSpace, Status: "running", Message: "Removing database volumes"}
				default:
					t.Errorf("unexpected API request %s", request.URL.Path)
					http.NotFound(writer, request)
					return
				}
				_ = json.NewEncoder(writer).Encode(output)
			}))
			assert.NoErr(err)
			response := httptest.NewRecorder()
			web.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/ui/page?view=postgres&organization=team&space=dev&postgresOperation=deletion", nil))
			if operationSpace != "dev" {
				assert.Equal(response.Code, http.StatusNotFound)
				return
			}
			assert.Equal(response.Code, http.StatusOK)
			body := response.Body.String()
			assert.True(strings.Contains(body, "Removing database volumes"))
			assert.Equal(decodeView[pageData](t, response, "workspace").Postgres.Operation.ID, "deletion")
			assert.True(strings.Contains(body, "postgresOperation=deletion"))
		})
	}
}

// TestManagedSettingsAndLogBoundaries protects exclusive Settings navigation, blank credential fields, explicit grants and inert untrusted database log output.
func TestManagedSettingsAndLogBoundaries(t *testing.T) {
	assert := is.New(t)
	offsiteEnabled := false
	web, err := NewHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var output any
		switch request.URL.Path {
		case "/api/v1/organizations":
			output = []appapi.Organization{{Name: "team", ManagedAddons: false}}
		case "/api/v1/organizations/team/clusters":
			output = []appapi.Cluster{}
		case "/api/v1/management/component-catalog":
			output = appapi.ComponentCatalog{Scope: "management", Namespace: "borq", Profile: "dev", Components: []appapi.ComponentConfiguration{{Name: "openbao", Values: map[string]any{"server": map[string]any{"enabled": true}}}}}
		case "/api/v1/management/s3":
			output = appapi.ManagementS3{CredentialsRef: "vault:shared/1", Region: "us-east-1", Bucket: "shared"}
		case "/api/v1/management/configuration":
			repository, reference, secret := "s3:https://offsite.example/backups/borq", "offsite-reference", "never-echo-offsite-secret"
			output = appapi.ManagementConfiguration{Offsite: &appapi.ManagementOffsiteInput{Enabled: offsiteEnabled, Repository: &repository, CredentialsRef: &reference, Password: &secret, AccessKey: &secret, SecretKey: &secret}}
		case "/api/v1/management/operations/offsite-change":
			offsiteEnabled = true
			output = appapi.Operation{ID: "offsite-change", Status: "succeeded"}
		case "/api/v1/organizations/team/spaces/dev/managed-addons/postgres/database/logs":
			assert.Equal(request.URL.Query().Get("limit"), "200")
			assert.Equal(request.URL.Query().Get("search"), "error")
			assert.Equal(request.URL.Query().Get("range"), "12h")
			assert.Equal(request.URL.Query().Get("end"), "2026-10-24T12:00:00Z")
			output = appapi.AppLogs{Entries: []appapi.AppLogEntry{{Message: "<script>private log</script>"}}}
		default:
			t.Errorf("unexpected API request %s", request.URL.Path)
			http.NotFound(writer, request)
			return
		}
		_ = json.NewEncoder(writer).Encode(output)
	}))
	assert.NoErr(err)
	response := httptest.NewRecorder()
	web.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/ui/page?view=settings&managementOperation=offsite-change", nil))

	page := decodeView[pageData](t, response, "workspace")
	assert.Equal(page.View, "infrastructure")
	assert.Equal(page.Settings.Selected.Name, "")
	assert.Equal(page.Settings.Storage.CredentialsRef, "vault:shared/1")
	assert.True(page.Settings.Offsite.Enabled)
	assert.Equal(*page.Settings.Offsite.Repository, "s3:https://offsite.example/backups/borq")
	assert.Equal(*page.Settings.Offsite.CredentialsRef, "offsite-reference")
	assert.True(!strings.Contains(response.Body.String(), "never-echo-offsite-secret"))
	assert.Equal(page.Organizations[0].ManagedAddons, false)
	response = httptest.NewRecorder()
	web.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/ui/postgres?organization=team&space=dev&postgres=database&panel=logs&search=error&range=12h&end=2026-10-24T12%3A00%3A00Z", nil))
	data := decodeView[postgresPanelData](t, response, "postgres-logs")
	assert.Equal(data.Logs.Entries[0].Message, "<script>private log</script>")
	assert.True(!strings.Contains(response.Body.String(), "<script>"))

}

// TestManagedBrowserMutationForwardsTypedIntent normalizes editable destination names and returns each mutation to its relevant section.
func TestManagedBrowserMutationForwardsTypedIntent(t *testing.T) {
	for _, test := range []struct {
		name, action, destination, forwarded, method, view, tab string
		apply                                                   bool
	}{
		{name: "create", action: "put-postgres", destination: "DataBASE", forwarded: "database", method: http.MethodPut, view: "postgres-detail", tab: "overview"},
		{name: "restore", action: "restore-postgres", destination: "ReSTored", forwarded: "restored", method: http.MethodPost, view: "postgres-detail", tab: "overview"},
		{name: "preserve invalid characters", action: "put-postgres", destination: "DÄTA_Name!", forwarded: "dÄta_name!", method: http.MethodPut, view: "postgres-detail", tab: "overview"},
		{name: "apply immutable name", action: "put-postgres", destination: "Existing", forwarded: "Existing", method: http.MethodPut, apply: true, view: "postgres-detail", tab: "configuration"},
		{name: "delete immutable name", action: "delete-postgres", destination: "Existing", forwarded: "Existing", method: http.MethodDelete, view: "postgres"},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			web := handler{api: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				assert.Equal(request.Method, test.method)
				path := "/api/v1/organizations/team/spaces/dev/managed-addons/postgres/" + test.forwarded
				if test.action == "restore-postgres" {
					path += "/restore"
					var input appapi.PostgresRestoreInput
					assert.NoErr(json.NewDecoder(request.Body).Decode(&input))
					assert.Equal(input.BackupID, "approved")
				} else if test.action == "put-postgres" {
					var input appapi.PostgresInput
					assert.NoErr(json.NewDecoder(request.Body).Decode(&input))
					assert.True(input.Instances == nil && input.Storage == nil)
					if test.apply {
						assert.True(input.Database != nil && input.Username != nil)
						assert.Equal(*input.Database, "AppDB")
						assert.Equal(*input.Username, "SQLUser")
					}
				}
				assert.Equal(request.URL.Path, path)
				assert.Equal(request.URL.Query().Get("apply") == "true", test.apply)
				writer.WriteHeader(http.StatusAccepted)
				_ = json.NewEncoder(writer).Encode(appapi.Postgres{Name: test.forwarded, Operation: "change"})
			})}
			form := url.Values{"action": {test.action}, "organization": {"team"}, "space": {"dev"}, "name": {test.destination}, "backup": {"approved"}, "tab": {"backups"}, "subtab": {"logs"}}
			if test.apply {
				form.Set("apply", "true")
				form.Set("configuration", "database: AppDB\nusername: SQLUser\n")
			}
			request := httptest.NewRequest(http.MethodPost, "/api/ui/actions", strings.NewReader(form.Encode()))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			assert.NoErr(request.ParseForm())
			result, err := web.execute(request)
			assert.NoErr(err)
			assert.Equal(result.selection.Get("view"), test.view)
			assert.Equal(result.selection.Get("tab"), test.tab)
			assert.Equal(result.selection.Get("subtab"), "")
			assert.Equal(result.selection.Get("organization"), "team")
			assert.Equal(result.selection.Get("space"), "dev")
			if test.action == "delete-postgres" {
				assert.Equal(result.selection.Get("postgres"), "")
				assert.Equal(result.selection.Get("postgresOperation"), "change")
			} else {
				assert.Equal(result.selection.Get("postgres"), test.forwarded)
			}
		})
	}
}

// TestManagedMetricsAvailability distinguishes unavailable SQL observations from measured zero activity.
func TestManagedMetricsAvailability(t *testing.T) {
	for _, available := range []bool{false, true} {
		t.Run(fmt.Sprint(available), func(t *testing.T) {
			assert := is.New(t)
			web, err := NewHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				_ = json.NewEncoder(writer).Encode(appapi.PostgresMetrics{ActivityAvailable: available})
			}))
			assert.NoErr(err)
			response := httptest.NewRecorder()
			web.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/ui/postgres?organization=team&space=dev&postgres=db&panel=metrics", nil))
			data := decodeView[postgresPanelData](t, response, "postgres-metrics")
			assert.Equal(data.Metrics.ActivityAvailable, available)
		})
	}
}

// TestManagedPostgresRestoreValidation rejects malformed recovery times and
// schema-invalid advanced settings before a database operation reaches the API.
func TestManagedPostgresRestoreValidation(t *testing.T) {
	for _, test := range []struct{ action, field, value, message string }{
		{"restore-postgres", "recoveryTarget", "yesterday", "Recovery time must use RFC3339"},
		{"put-postgres", "configuration", "instances: many", "instances"},
		{"put-postgres", "configuration", "unknown: do-not-echo-secret", ""},
	} {
		t.Run(test.field, func(t *testing.T) {
			assert := is.New(t)
			web, err := NewHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("invalid database intent reached API") }))
			assert.NoErr(err)
			form := url.Values{"action": {test.action}, "organization": {"team"}, "space": {"dev"}, "name": {"database"}}
			form.Set(test.field, test.value)
			response := postForm(web, form)
			assert.Equal(response.Code, http.StatusBadRequest)
			var problem appapi.Problem
			assert.NoErr(json.Unmarshal(response.Body.Bytes(), &problem))
			assert.True(strings.Contains(problem.Message, test.message))
			assert.True(!strings.Contains(response.Body.String(), "do-not-echo-secret"))
		})
	}
}
