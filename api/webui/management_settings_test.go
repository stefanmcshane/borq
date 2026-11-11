package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/api/appapi"
)

// TestBackupCoverageSettings keeps slow archive inspection opt-in and displays
// safe inspection errors without discarding the rest of the settings page.
func TestBackupCoverageSettings(t *testing.T) {
	for _, mode := range []string{"idle", "checked", "unavailable"} {
		t.Run(mode, func(t *testing.T) {
			assert := is.New(t)
			inspections := 0
			web := handler{api: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				assert.Equal(request.Method, http.MethodGet)
				var output any
				switch request.URL.Path {
				case "/api/v1/management/s3":
					output = appapi.ManagementS3{}
				case "/api/v1/management/configuration":
					output = appapi.ManagementConfiguration{}
				case "/api/v1/management/backups/status":
					inspections++
					if mode == "unavailable" {
						writer.WriteHeader(http.StatusServiceUnavailable)
						_, _ = writer.Write([]byte(`{"message":"private-provider-error"}`))
						return
					}
					output = appapi.ManagementBackupCoverage{CheckedAt: time.Now().UTC(), State: "attention", Components: []appapi.BackupCoverageComponent{{Kind: "management", Name: "sqlite", State: "disabled", Message: "Enable management backups."}}}
				default:
					t.Fatalf("unexpected API request %s", request.URL.Path)
				}
				_ = json.NewEncoder(writer).Encode(output)
			})}
			address := "/api/ui/page?view=settings"
			if mode != "idle" {
				address += "&checkBackups=true"
			}
			request := httptest.NewRequest(http.MethodGet, address, nil)
			page, err := web.loadManagementSettings(request, pageData{Infrastructure: infrastructureSelection{Node: managementNode, Tab: "settings"}})
			assert.NoErr(err)
			if mode == "idle" {
				assert.Equal(inspections, 0)
				assert.True(page.Settings.BackupCoverage == nil)
				return
			}
			assert.Equal(inspections, 1)
			if mode == "checked" {
				assert.True(page.Settings.BackupCoverage != nil)
				assert.Equal(string(page.Settings.BackupCoverage.State), "attention")
			} else {
				assert.True(page.Settings.BackupCoverage == nil)
				assert.True(strings.Contains(page.Settings.BackupCoverageError, "could not be checked"))
				assert.True(!strings.Contains(page.Settings.BackupCoverageError, "private-provider-error"))
			}
		})
	}
}

// TestOffsiteSettingsMutation keeps offsite policy changes separate from primary
// storage and resolves saved credentials without trusting browser references.
func TestOffsiteSettingsMutation(t *testing.T) {
	for _, mode := range []string{"stored", "replace", "disable", "http", "legacy"} {
		t.Run(mode, func(t *testing.T) {
			assert := is.New(t)
			web := handler{api: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				assert.Equal(request.URL.Path, "/api/v1/management/configuration")
				if request.Method == http.MethodGet {
					assert.True(mode != "replace" && mode != "disable")
					reference := "saved-reference"
					_ = json.NewEncoder(writer).Encode(appapi.ManagementConfiguration{Offsite: &appapi.ManagementOffsiteInput{CredentialsRef: &reference}})
					return
				}
				assert.Equal(request.Method, http.MethodPut)
				var body appapi.ManagementConfiguration
				assert.NoErr(json.NewDecoder(request.Body).Decode(&body))
				assert.True(body.S3 == nil && body.Backups == nil && body.Components == nil && body.Organizations == nil)
				assert.True(body.Offsite != nil)
				policy := *body.Offsite
				if mode == "disable" {
					assert.Equal(policy, appapi.ManagementOffsiteInput{Enabled: false})
				} else {
					assert.True(policy.Enabled)
					repository := "s3:https://offsite.example/backups/borq"
					if mode == "http" {
						repository = "s3:http://offsite.example/backups/borq"
					}
					assert.Equal(*policy.Repository, repository)
					assert.Equal(*policy.Region, "region-one")
					assert.Equal(*policy.Interval, "12h")
					assert.Equal(*policy.KeepLast, 20)
					if mode == "replace" {
						assert.True(policy.CredentialsRef == nil)
						assert.Equal(*policy.Password, "private-password")
						assert.Equal(*policy.AccessKey, "private-access")
						assert.Equal(*policy.SecretKey, "private-secret")
						assert.Equal(*policy.SessionToken, "private-session")
					} else {
						assert.Equal(*policy.CredentialsRef, "saved-reference")
						assert.True(policy.Password == nil && policy.AccessKey == nil && policy.SecretKey == nil && policy.SessionToken == nil)
					}
				}
				writer.WriteHeader(http.StatusAccepted)
				_ = json.NewEncoder(writer).Encode(appapi.Operation{ID: "offsite-change"})
			})}
			form := url.Values{"view": {"infrastructure"}, "node": {"management"}, "tab": {"settings"}, "componentScope": {"postgres"}, "action": {"put-management-offsite"}, "enabled": {"on"}, "repository": {"https://offsite.example/backups/borq"}, "region": {"region-one"}, "interval": {"12h"}, "keepLast": {"20"}, "credentialsRef": {"untrusted-client-reference"}}
			if mode == "replace" {
				for key, value := range map[string]string{"password": "private-password", "accessKey": "private-access", "secretKey": "private-secret", "sessionToken": "private-session"} {
					form.Set(key, value)
				}
			} else if mode == "disable" {
				form.Set("action", "disable-management-offsite")
			} else if mode == "http" {
				form.Set("repository", "http://offsite.example/backups/borq")
			} else if mode == "legacy" {
				form.Set("repository", "s3:https://offsite.example/backups/borq")
			}
			request := httptest.NewRequest(http.MethodPost, "/api/ui/actions", nil)
			request.PostForm = form
			result, err := web.execute(request)
			assert.NoErr(err)
			assert.Equal(result.selection.Get("view"), "infrastructure")
			assert.Equal(result.selection.Get("node"), "management")
			assert.Equal(result.selection.Get("tab"), "settings")
			assert.Equal(result.selection.Get("managementOperation"), "offsite-change")
		})
	}
}

// TestOffsiteSettingsNeedsCredentials prevents an unconfigured form from choosing
// a vault reference supplied by the browser instead of entering credentials.
func TestOffsiteSettingsNeedsCredentials(t *testing.T) {
	assert := is.New(t)
	web := handler{api: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(request.Method, http.MethodGet)
		assert.Equal(request.URL.Path, "/api/v1/management/configuration")
		_ = json.NewEncoder(writer).Encode(appapi.ManagementConfiguration{})
	})}
	request := httptest.NewRequest(http.MethodPost, "/api/ui/actions", nil)
	request.PostForm = url.Values{"action": {"put-management-offsite"}, "credentialsRef": {"untrusted-client-reference"}}
	_, err := web.execute(request)
	assert.True(err != nil)
	assert.Equal(err.Error(), "Enter the backup encryption password, access key, and secret key before saving offsite backups.")
}

// TestPrimaryStorageCredentials keeps vault references internal when reusing
// primary credentials and sends replacement keys only when the user supplies them.
func TestPrimaryStorageCredentials(t *testing.T) {
	for _, replace := range []bool{false, true} {
		assert := is.New(t)
		web := handler{api: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			assert.Equal(request.URL.Path, "/api/v1/management/s3")
			if request.Method == http.MethodGet {
				assert.True(!replace)
				_ = json.NewEncoder(writer).Encode(appapi.ManagementS3{CredentialsRef: "saved-primary-reference"})
				return
			}
			assert.Equal(request.Method, http.MethodPut)
			var body appapi.ManagementS3Input
			assert.NoErr(json.NewDecoder(request.Body).Decode(&body))
			if replace {
				assert.True(body.CredentialsRef == nil)
				assert.Equal(*body.AccessKey, "private-access")
				assert.Equal(*body.SecretKey, "private-secret")
			} else {
				assert.Equal(*body.CredentialsRef, "saved-primary-reference")
				assert.True(body.AccessKey == nil && body.SecretKey == nil)
			}
			writer.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(writer).Encode(appapi.Operation{ID: "primary-storage-test", Status: "queued"})
		})}
		request := httptest.NewRequest(http.MethodPost, "/api/ui/actions", nil)
		request.PostForm = url.Values{"action": {"put-management-s3"}, "credentialsRef": {"untrusted-client-reference"}}
		if replace {
			request.PostForm.Set("accessKey", "private-access")
			request.PostForm.Set("secretKey", "private-secret")
		}
		result, err := web.execute(request)
		assert.NoErr(err)
		assert.Equal(result.selection.Get("managementOperation"), "primary-storage-test")
		assert.Equal(result.notice, "Primary S3 connection test queued. Settings will be saved after the test succeeds.")
	}
}

// TestOffsiteSettingsInvalidInput rejects incomplete replacement credentials and
// invalid retention without forwarding secrets or altering the saved policy.
func TestOffsiteSettingsInvalidInput(t *testing.T) {
	for _, form := range []url.Values{
		{"password": {"never-echo-private-password"}},
		{"sessionToken": {"never-echo-private-session"}},
		{"keepLast": {"many"}},
		{"keepLast": {"0"}},
	} {
		assert := is.New(t)
		web := handler{api: http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("invalid offsite form reached API") })}
		form.Set("action", "put-management-offsite")
		request := httptest.NewRequest(http.MethodPost, "/api/ui/actions", nil)
		request.PostForm = form
		_, err := web.execute(request)
		assert.True(err != nil)
		problem, ok := err.(apiProblem)
		assert.True(ok)
		assert.Equal(problem.status, http.StatusBadRequest)
		assert.True(!strings.Contains(problem.message, "never-echo"))
	}
}

// TestComponentCatalogView renders desired metadata and YAML separately from the saved override editor in the chosen target context.
func TestComponentCatalogView(t *testing.T) {
	assert := is.New(t)
	raw := map[string]any{"savedOverride": "only-this-is-editable"}
	web, err := NewHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var output any
		switch request.URL.Path {
		case "/api/v1/organizations":
			output = []appapi.Organization{{Name: "first"}, {Name: "selected"}}
		case "/api/v1/organizations/first/clusters":
			output = []appapi.Cluster{{Name: "default"}}
		case "/api/v1/organizations/selected/clusters":
			output = []appapi.Cluster{{Name: "remote"}}
		case "/api/v1/organizations/first/spaces", "/api/v1/organizations/selected/spaces":
			output = []appapi.Space{}
		case "/api/v1/management/s3":
			output = appapi.ManagementS3{}
		case "/api/v1/management/component-catalog":
			if request.URL.Query().Get("scope") == "postgres" {
				output = appapi.ComponentCatalog{Scope: "postgres", Namespace: "database-namespace", Profile: "database-profile", Components: []appapi.ComponentConfiguration{{Name: "cloudnative-pg", Values: map[string]any{"databaseDefault": "controller-values"}}, {Name: "plugin-barman-cloud", Values: map[string]any{"secret": "unselected-component-private-value"}}}}
				break
			}
			assert.Equal(request.URL.Query().Get("organization"), "selected")
			assert.Equal(request.URL.Query().Get("cluster"), "remote")
			output = appapi.ComponentCatalog{Scope: "target", Namespace: "custom-namespace", Profile: "production", Components: []appapi.ComponentConfiguration{
				{Name: "cert-manager", Chart: "cert-manager", Version: "1.0.0", Values: map[string]any{"enabled": true, "secret": "unselected-component-private-value"}},
				{Name: "alloy", Description: "Collects telemetry.", Repository: "https://charts.example", Chart: "collector", Version: "2.3.4", Values: map[string]any{"borqDefault": "rendered-for-selected-remote", "savedOverride": "only-this-is-editable"}, Override: appapi.ComponentOverride{Values: &raw}},
			}}
		case "/api/v1/management/operations/apply":
			output = appapi.Operation{ID: "apply", Status: "running"}
		default:
			t.Fatalf("unexpected API request %s", request.URL)
		}
		_ = json.NewEncoder(writer).Encode(output)
	}))
	assert.NoErr(err)
	response := httptest.NewRecorder()
	web.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/ui/page?view=settings&componentScope=target&componentOrganization=selected&componentCluster=remote&component=alloy&managementOperation=apply", nil))

	page := decodeView[pageData](t, response, "workspace")
	settings := page.Settings
	assert.Equal(page.Infrastructure.ComponentScope, "target")
	assert.Equal(page.Infrastructure.Organization, "selected")
	assert.Equal(page.Infrastructure.Cluster, "remote")
	assert.Equal(settings.Catalog.Namespace, "custom-namespace")
	assert.Equal(settings.Catalog.Profile, "production")
	assert.Equal(settings.Selected.Name, "alloy")
	assert.Equal(settings.Selected.Repository, "https://charts.example")
	assert.Equal(settings.Selected.Chart, "collector")
	assert.Equal(settings.Selected.Version, "2.3.4")
	assert.Equal(settings.Selected.Values, map[string]any{"borqDefault": "rendered-for-selected-remote", "savedOverride": "only-this-is-editable"})
	assert.Equal(*settings.Selected.Override.Values, raw)
	assert.Equal(len(settings.Components), 4)
	assert.True(!strings.Contains(response.Body.String(), "unselected-component-private-value"))
	for _, component := range settings.Catalog.Components {
		assert.True(component.Values == nil)
		assert.True(component.Override.Values == nil)
	}
	assert.True(!settings.Components[0].Selected)
	assert.True(settings.Components[1].Selected)

	next, err := url.Parse(settings.Components[0].URL)
	assert.NoErr(err)
	assert.Equal(next.Query().Get("component"), "cert-manager")
	assert.Equal(next.Query().Get("managementOperation"), "apply")
	page = requestPage(t, web, next.RawQuery, http.StatusOK)
	assert.Equal(page.Settings.Selected.Name, "cert-manager")
	location, err := url.Parse(page.Location)
	assert.NoErr(err)
	assert.Equal(location.Query().Get("component"), "cert-manager")
	assert.Equal(location.Query().Get("organization"), "selected")
	assert.Equal(location.Query().Get("cluster"), "remote")
	assert.Equal(location.Query().Get("managementOperation"), "apply")

	databasePage := requestPage(t, web, "view=infrastructure&node=application&tab=components&organization=selected&cluster=remote&componentScope=postgres&component=cloudnative-pg", http.StatusOK)
	assert.Equal(databasePage.Settings.Selected.Name, "cloudnative-pg")
	assert.Equal(databasePage.Settings.Catalog.Namespace, "database-namespace")
	assert.Equal(len(databasePage.Settings.Components), 4)
	assert.Equal(databasePage.Settings.Selected.Values, map[string]any{"databaseDefault": "controller-values"})

}

// TestComponentCatalogMutation preserves preview selection and sends only explicit edits, including the clear-values object.
func TestComponentCatalogMutation(t *testing.T) {
	for _, values := range []string{"", "{}", "custom: changed"} {
		t.Run(values, func(t *testing.T) {
			assert := is.New(t)
			web := handler{api: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				assert.Equal(request.Method, http.MethodPut)
				assert.Equal(request.URL.Path, "/api/v1/management/configuration")
				var body appapi.ManagementConfiguration
				assert.NoErr(json.NewDecoder(request.Body).Decode(&body))
				assert.Equal(len(*body.Components), 1)
				override := (*body.Components)["alloy"]
				assert.True(override.Repo == nil && override.Chart == nil && override.Version == nil)
				if values == "" {
					assert.True(override.Values == nil)
				} else if values == "{}" {
					assert.True(override.Values != nil)
					assert.Equal(len(*override.Values), 0)
				} else {
					assert.Equal(*override.Values, map[string]any{"custom": "changed"})
				}
				writer.WriteHeader(http.StatusAccepted)
				_ = json.NewEncoder(writer).Encode(appapi.Operation{ID: "accepted"})
			})}
			form := url.Values{"view": {"settings"}, "action": {"put-management-component"}, "componentScope": {"target"}, "componentOrganization": {"selected"}, "componentCluster": {"remote"}, "component": {"alloy"}, "values": {values}}
			request := httptest.NewRequest(http.MethodPost, "/api/ui/actions", strings.NewReader(form.Encode()))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			assert.NoErr(request.ParseForm())
			result, err := web.execute(request)
			assert.NoErr(err)
			for _, key := range []string{"componentScope", "component"} {
				assert.Equal(result.selection.Get(key), form.Get(key))
			}
			assert.Equal(result.selection.Get("organization"), "selected")
			assert.Equal(result.selection.Get("cluster"), "remote")
			assert.Equal(result.selection.Get("managementOperation"), "accepted")
		})
	}
}

// TestComponentCatalogNoTargets avoids inventing target runtime values before a deployment cluster is registered.
func TestComponentCatalogNoTargets(t *testing.T) {
	assert := is.New(t)
	web, err := NewHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var output any
		switch request.URL.Path {
		case "/api/v1/organizations":
			output = []appapi.Organization{{Name: "team"}}
		case "/api/v1/organizations/team/clusters":
			output = []appapi.Cluster{}
		case "/api/v1/management/s3":
			output = appapi.ManagementS3{}
		default:
			t.Fatalf("unexpected request %s", request.URL)
		}
		_ = json.NewEncoder(writer).Encode(output)
	}))
	assert.NoErr(err)
	response := httptest.NewRecorder()
	web.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/ui/page?view=settings&componentScope=target", nil))
	assert.Equal(response.Code, http.StatusOK)
	page := decodeView[pageData](t, response, "workspace")
	assert.Equal(page.Infrastructure.Node, managementNode)
	assert.Equal(page.Infrastructure.Tab, "clusters")
	assert.Equal(len(page.Settings.Components), 0)
	assert.Equal(page.Settings.Selected.Name, "")
}

// TestInfrastructureMutationContext rejects incompatible scopes before accepting any configuration write.
func TestInfrastructureMutationContext(t *testing.T) {
	for _, input := range []struct {
		query   string
		status  int
		message string
	}{
		{"view=infrastructure&node=application&organization=selected&cluster=remote&componentScope=management", http.StatusNotFound, "component source or section"},
		{"view=infrastructure&node=application&cluster=remote&componentScope=target", http.StatusBadRequest, "Choose an organization"},
	} {
		t.Run(input.query, func(t *testing.T) {
			assert := is.New(t)
			web, err := NewHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("invalid scope reached protected API") }))
			assert.NoErr(err)
			form, err := url.ParseQuery(input.query)
			assert.NoErr(err)
			form.Set("action", "put-management-component")
			form.Set("component", "alloy")
			form.Set("values", "private: never-echo")
			response := postForm(web, form)
			assert.Equal(response.Code, input.status)
			assert.True(strings.Contains(response.Body.String(), input.message))
			assert.True(!strings.Contains(response.Body.String(), "never-echo"))
		})
	}
}
