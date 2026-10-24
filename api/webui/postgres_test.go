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

// TestManagedPostgresBrowser keeps restore choices restricted to eligible backups and credentials outside rendered database pages.
func TestManagedPostgresBrowser(t *testing.T) {
	assert := is.New(t)
	reason := "incomplete metadata"
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
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
			output = appapi.Postgres{Name: "database", State: "ready", Protection: appapi.PostgresProtection{Backups: "ready", Wal: "ready", Snapshots: "unsupported"}, RestoreCheck: appapi.PostgresRestoreCheck{Status: "succeeded"}}
		case strings.HasSuffix(request.URL.Path, "/backups"):
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
	web.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/?view=postgres-detail&organization=team&space=dev&postgres=database", nil))
	assert.Equal(response.Code, http.StatusOK)
	assert.Equal(response.Header().Get("Cache-Control"), "no-store")
	body := response.Body.String()
	document, err := html.Parse(strings.NewReader(body))
	assert.NoErr(err)
	options := []string{}
	for node := range document.Descendants() {
		if node.Data == "option" {
			for _, attribute := range node.Attr {
				if attribute.Key == "value" {
					options = append(options, attribute.Val)
				}
			}
		}
	}
	assert.True(containsText(options, "approved"))
	assert.True(!containsText(options, "rejected"))
	for _, text := range []string{"unsupported", "Recovery verification", "borq am postgres env"} {
		if !strings.Contains(body, text) {
			t.Errorf("missing database UI text %q", text)
		}
	}
	assert.True(!strings.Contains(body, "PGPASSWORD"))
}

// TestManagedSettingsAndLogBoundaries protects blank credential fields, explicit grants and inert untrusted database log output.
func TestManagedSettingsAndLogBoundaries(t *testing.T) {
	assert := is.New(t)
	web, err := NewHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var output any
		switch request.URL.Path {
		case "/api/v1/organizations":
			output = []appapi.Organization{{Name: "team", ManagedAddons: false}}
		case "/api/v1/management/configuration":
			output = appapi.ManagementConfiguration{}
		case "/api/v1/management/s3":
			output = appapi.ManagementS3{CredentialsRef: "vault:shared/1", Region: "us-east-1", Bucket: "shared"}
		case "/api/v1/organizations/team/spaces/dev/managed-addons/postgres/database/logs":
			assert.Equal(request.URL.Query().Get("limit"), "200")
			assert.Equal(request.URL.Query().Get("search"), "error")
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
	document, err := html.Parse(response.Body)
	assert.NoErr(err)
	for node := range document.Descendants() {
		attributes := map[string]string{}
		for _, attribute := range node.Attr {
			attributes[attribute.Key] = attribute.Val
		}
		if attributes["name"] == "secretKey" || attributes["name"] == "accessKey" || attributes["name"] == "sessionToken" {
			assert.Equal(attributes["value"], "")
		}
		if attributes["name"] == "enabled" {
			_, checked := attributes["checked"]
			assert.True(!checked)
		}
	}
	response = httptest.NewRecorder()
	web.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/ui/postgres?organization=team&space=dev&postgres=database&panel=logs&search=error", nil))
	assert.Equal(response.Code, http.StatusOK)
	assert.True(!strings.Contains(response.Body.String(), "<script>"))
	assert.True(strings.Contains(response.Body.String(), "&lt;script&gt;private log&lt;/script&gt;"))
}

// TestManagedBrowserMutationForwardsTypedIntent keeps database creation name-only and storage secrets in the explicit write request.
func TestManagedBrowserMutationForwardsTypedIntent(t *testing.T) {
	assert := is.New(t)
	web := handler{api: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var input appapi.PostgresInput
		assert.NoErr(json.NewDecoder(request.Body).Decode(&input))
		assert.Equal(request.URL.Path, "/api/v1/organizations/team/spaces/dev/managed-addons/postgres/database")
		assert.True(input.Instances == nil && input.Storage == nil)
		writer.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(writer).Encode(appapi.Postgres{Name: "database"})
	})}
	request := httptest.NewRequest(http.MethodPost, "/ui/actions", strings.NewReader(url.Values{"action": {"put-postgres"}, "organization": {"team"}, "space": {"dev"}, "name": {"database"}}.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	assert.NoErr(request.ParseForm())
	result, err := web.execute(request)
	assert.NoErr(err)
	assert.Equal(result.selection.Get("postgres"), "database")
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
