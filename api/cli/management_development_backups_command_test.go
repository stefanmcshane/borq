package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/api/appapi"
)

// TestDevelopmentBackupDefaults owns missing-policy defaults and preservation of explicit choices at the CLI boundary.
func TestDevelopmentBackupDefaults(t *testing.T) {
	for _, fixture := range []struct {
		name          string
		configuration appapi.ManagementConfiguration
	}{
		{name: "fresh"},
		{name: "local explicitly disabled", configuration: appapi.ManagementConfiguration{Backups: &appapi.ManagementBackupInput{Enabled: false}}},
		{name: "offsite explicitly enabled", configuration: appapi.ManagementConfiguration{Offsite: &appapi.ManagementOffsiteInput{Enabled: true}}},
		{name: "both configured", configuration: appapi.ManagementConfiguration{Backups: &appapi.ManagementBackupInput{Enabled: true}, Offsite: &appapi.ManagementOffsiteInput{Enabled: true}}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			assert := is.New(t)
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			var applied *appapi.ManagementConfiguration
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				assert.Equal(request.URL.Path, "/api/v1/management/configuration")
				writer.Header().Set("Content-Type", "application/json")
				if request.Method == http.MethodGet {
					assert.NoErr(json.NewEncoder(writer).Encode(fixture.configuration))
					return
				}
				assert.Equal(request.Method, http.MethodPut)
				assert.NoErr(json.NewDecoder(request.Body).Decode(&applied))
				writer.WriteHeader(http.StatusAccepted)
				_, _ = writer.Write([]byte(`{"id":"dev-backups","status":"succeeded"}`))
			}))
			defer server.Close()
			_, _, err := runConfiguredApplicationCommand("management", "backups", "local", "configure-dev", "--api-url", server.URL)
			assert.NoErr(err)
			if fixture.configuration.Backups != nil && fixture.configuration.Offsite != nil {
				assert.True(applied == nil)
			} else {
				assert.True(applied != nil)
				assert.True(applied.S3 == nil && applied.Components == nil && applied.Organizations == nil)
				if fixture.configuration.Offsite == nil {
					assert.True(applied.Offsite != nil && !applied.Offsite.Enabled)
				} else {
					assert.True(applied.Offsite == nil)
				}
			}
			if fixture.configuration.Backups != nil {
				if applied != nil {
					assert.True(applied.Backups == nil)
				}
				return
			}
			assert.True(applied.Backups.Enabled)
			assert.Equal(*applied.Backups.Interval, "1h")
			assert.Equal(*applied.Backups.KeepLast, 24)
			assert.True(applied.Backups.EncryptionKey == nil)
		})
	}
}
