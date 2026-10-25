package webui

import (
	"encoding/json"
	"fmt"
	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/api/appapi"
	"golang.org/x/net/html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestManagedPostgresBrowser keeps database sections scoped, restore choices eligible and credentials outside rendered pages.
func TestManagedPostgresBrowser(t *testing.T) {
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
					output = appapi.Postgres{Name: "database", EnvironmentGroup: "postgres-database-immutable-id", State: "ready", Operation: "change", Protection: appapi.PostgresProtection{Status: "ready"}, RestoreCheck: appapi.PostgresRestoreCheck{Status: "succeeded"}}
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
			web.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/?view=postgres-detail&organization=team&space=dev&postgres=database"+test.query, nil))
			if test.status != 0 {
				assert.Equal(response.Code, test.status)
				assert.True(!backupsRead)
				return
			}
			assert.Equal(response.Code, http.StatusOK)
			assert.Equal(response.Header().Get("Cache-Control"), "no-store")
			body := response.Body.String()
			assert.True(strings.Contains(body, test.content))
			assert.True(strings.Contains(body, "Updating database"))
			assert.True(!strings.Contains(body, "PGPASSWORD"))
			assert.Equal(backupsRead, test.tab == "backups")
			document, err := html.Parse(strings.NewReader(body))
			assert.NoErr(err)
			options := []string{}
			active := []string{}
			panels := []string{}
			groups := []string{}
			hidden := url.Values{}
			for node := range document.Descendants() {
				attributes := map[string]string{}
				for _, attribute := range node.Attr {
					attributes[attribute.Key] = attribute.Val
				}
				if node.Data == "option" {
					options = append(options, attributes["value"])
				}
				if node.Data == "input" && attributes["type"] == "hidden" {
					hidden.Set(attributes["name"], attributes["value"])
				}
				if node.Data == "a" && attributes["aria-current"] == "page" && strings.Contains(attributes["href"], "postgres-detail") {
					link, err := url.Parse(attributes["href"])
					assert.NoErr(err)
					assert.Equal(link.Query().Get("organization"), "team")
					assert.Equal(link.Query().Get("space"), "dev")
					assert.Equal(link.Query().Get("postgres"), "database")
					assert.Equal(attributes["hx-get"], attributes["href"])
					active = append(active, link.Query().Get("tab")+"/"+link.Query().Get("subtab"))
				}
				if node.Data == "a" && navigationText(node) == "postgres-database-immutable-id" {
					link, err := url.Parse(attributes["href"])
					assert.NoErr(err)
					assert.Equal(link.Query().Get("view"), "environment-group")
					assert.Equal(link.Query().Get("organization"), "team")
					assert.Equal(link.Query().Get("space"), "dev")
					assert.Equal(attributes["hx-get"], attributes["href"])
					groups = append(groups, link.Query().Get("group"))
				}
				if strings.HasPrefix(attributes["hx-get"], "/ui/postgres?") {
					link, err := url.Parse(attributes["hx-get"])
					assert.NoErr(err)
					panels = append(panels, link.Query().Get("panel"))
					assert.Equal(link.Query().Get("postgres"), "database")
					if test.subtab == "logs" {
						assert.Equal(link.Query().Get("range"), "12h")
						assert.Equal(link.Query().Get("search"), "error")
						assert.Equal(link.Query().Get("end"), "2026-10-24T12:00:00Z")
					}
				}
			}
			assert.True(containsText(active, test.tab+"/"))
			if test.subtab != "" {
				assert.True(containsText(active, test.tab+"/"+test.subtab))
			}
			if test.panel == "" {
				assert.Equal(len(panels), 0)
			} else {
				assert.Equal(panels, []string{test.panel})
			}
			if test.tab == "backups" {
				assert.True(containsText(options, "approved"))
				assert.True(!containsText(options, "rejected"))
			}
			if test.tab == "backups" || test.tab == "configuration" {
				assert.Equal(hidden.Get("tab"), test.tab)
			}
			if test.tab == "overview" {
				assert.True(strings.Contains(body, "Backup protection"))
				assert.True(strings.Contains(body, "borq am postgres env"))
				assert.True(strings.Contains(body, "Environment group selector"))
				assert.Equal(groups, []string{"postgres-database-immutable-id"})
			} else {
				assert.Equal(groups, []string{})
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
			web.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/?view=postgres&organization=team&space=dev&postgresOperation=deletion", nil))
			if operationSpace != "dev" {
				assert.Equal(response.Code, http.StatusNotFound)
				return
			}
			assert.Equal(response.Code, http.StatusOK)
			body := response.Body.String()
			assert.True(strings.Contains(body, "Removing database volumes"))
			assert.True(strings.Contains(body, `hx-get="/ui/postgres/operations/deletion"`))
			assert.True(strings.Contains(body, "postgresOperation=deletion"))
		})
	}
}

// TestManagedSettingsAndLogBoundaries protects exclusive Settings navigation, blank credential fields, explicit grants and inert untrusted database log output.
func TestManagedSettingsAndLogBoundaries(t *testing.T) {
	assert := is.New(t)
	web, err := NewHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var output any
		switch request.URL.Path {
		case "/api/v1/organizations":
			output = []appapi.Organization{{Name: "team", ManagedAddons: false}}
		case "/api/v1/management/component-catalog":
			output = appapi.ComponentCatalog{Scope: "management", Namespace: "borq", Profile: "dev", Components: []appapi.ComponentConfiguration{{Name: "openbao", Values: map[string]any{"server": map[string]any{"enabled": true}}}}}
		case "/api/v1/management/s3":
			output = appapi.ManagementS3{CredentialsRef: "vault:shared/1", Region: "us-east-1", Bucket: "shared"}
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
	web.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/?view=settings", nil))
	assert.Equal(response.Code, http.StatusOK)
	assert.True(strings.Contains(response.Body.String(), `name="component" value="openbao"`))
	document, err := html.Parse(response.Body)
	assert.NoErr(err)
	var selectedNavigation []string
	for node := range document.Descendants() {
		attributes := map[string]string{}
		for _, attribute := range node.Attr {
			attributes[attribute.Key] = attribute.Val
		}
		if node.Data == "a" && strings.Contains(attributes["class"], "nav-item") && attributes["aria-current"] == "page" {
			selectedNavigation = append(selectedNavigation, navigationText(node))
		}
		if attributes["name"] == "secretKey" || attributes["name"] == "accessKey" || attributes["name"] == "sessionToken" {
			assert.Equal(attributes["value"], "")
		}
		if attributes["name"] == "enabled" {
			_, checked := attributes["checked"]
			assert.True(!checked)
		}
	}
	assert.Equal(selectedNavigation, []string{"Settings"})
	response = httptest.NewRecorder()
	web.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/ui/postgres?organization=team&space=dev&postgres=database&panel=logs&search=error&range=12h&end=2026-10-24T12%3A00%3A00Z", nil))
	assert.Equal(response.Code, http.StatusOK)
	assert.True(!strings.Contains(response.Body.String(), "<script>"))
	assert.True(strings.Contains(response.Body.String(), "&lt;script&gt;private log&lt;/script&gt;"))
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
					assert.True(input.Backup != nil && input.Backup.Enabled != nil && !*input.Backup.Enabled)
					assert.True(input.Backup.RestoreCheck != nil && input.Backup.RestoreCheck.Schedule != nil && *input.Backup.RestoreCheck.Schedule == "0 4 * * 2")
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
			form := url.Values{"backupSettings": {"true"}, "backups": {"false"}, "restoreTestSchedule": {"0 4 * * 2"}, "action": {test.action}, "organization": {"team"}, "space": {"dev"}, "name": {test.destination}, "backup": {"approved"}, "tab": {"backups"}, "subtab": {"logs"}}
			if test.apply {
				form.Set("apply", "true")
				form.Set("configuration", "database: AppDB\nusername: SQLUser\n")
			}
			request := httptest.NewRequest(http.MethodPost, "/ui/actions", strings.NewReader(form.Encode()))
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
			web, err := NewHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				_ = json.NewEncoder(writer).Encode(appapi.PostgresMetrics{ActivityAvailable: available})
			}))
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			web.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/ui/postgres?organization=team&space=dev&postgres=db&panel=metrics", nil))
			want := "<dt>Connections</dt><dd>Unavailable</dd>"
			if available {
				want = "<dt>Connections</dt><dd>0</dd>"
			}
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), want) {
				t.Fatalf("SQL availability %t was not rendered correctly", available)
			}
		})
	}
}

func containsText(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
