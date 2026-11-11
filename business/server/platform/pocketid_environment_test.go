package platform

import (
	"strings"
	"testing"

	"github.com/matryer/is"
)

// TestPocketIdEnvironment owns direct setting admission. It rejects alternate
// credentials, listeners, storage, loaders and logs before provisioning can start.
func TestPocketIdEnvironment(t *testing.T) {
	for _, name := range []string{
		"APP_URL", "INTERNAL_APP_URL", "DB_CONNECTION_STRING", "ENCRYPTION_KEY", "STATIC_API_KEY", "HOST", "PORT",
		"UNIX_SOCKET", "SYSTEMD_SOCKET", "TLS_CERT", "TLS_KEY", "PUID", "PGID", "UPLOAD_PATH", "FILE_BACKEND", "S3_ENDPOINT",
		"AUTO_CREATE_OIDC_CLIENT_SECRET", "UI_CONFIG_DISABLED", "ALLOW_INSECURE_CALLBACK_URLS", "LD_PRELOAD", "ENV", "BASH_ENV",
		"PATH", "LOG_LEVEL", "OTEL_EXPORTER_OTLP_ENDPOINT", "SMTP_PASSWORD_FILE", "LDAP_BIND_PASSWORD_FILE", "APP_NAME_FILE", "UNKNOWN",
	} {
		t.Run(name, func(t *testing.T) {
			is := is.New(t)
			err := ValidatePocketIdEnvironment(map[string]string{name: "do-not-print"})
			is.True(err != nil)
			is.True(strings.Contains(err.Error(), "unsupported or managed setting"))
			is.True(!strings.Contains(err.Error(), "do-not-print"))
		})
	}
	for name, value := range map[string]string{
		"too large": strings.Repeat("x", 128*1024), "NUL": "secret\x00suffix", "invalid UTF-8": string([]byte{0xff}),
	} {
		t.Run(name, func(t *testing.T) {
			is := is.New(t)
			is.True(ValidatePocketIdEnvironment(map[string]string{"SMTP_PASSWORD": value}) != nil)
		})
	}
	is := is.New(t)
	is.NoErr(ValidatePocketIdEnvironment(map[string]string{
		"HOME_PAGE_URL": "https://borq.example.test", "EMAIL_LOGIN_NOTIFICATION_MODE": "always", "EMAIL_VERIFICATION": "required",
		"EMAIL_LOGIN_NOTIFICATION_ENABLED": "true", "EMAIL_VERIFICATION_ENABLED": "false", "WEBAUTHN_ALLOW_SYNCED_PASSKEYS": "true",
		"SMTP_PASSWORD": "literal {{ password }}\nwith: punctuation", "LDAP_ENABLED": "false", "LDAP_URL": "ldaps://directory.example.test",
		"LDAP_BIND_PASSWORD": "secret", "LDAP_ATTRIBUTE_USER_UNIQUE_IDENTIFIER": "entryUUID", "LDAP_ADMIN_GROUP_NAME": "admins",
		"CIMD_URL_ALLOWLIST": `["https://client.example.test/*"]`,
	}))
}
