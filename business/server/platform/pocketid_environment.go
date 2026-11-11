package platform

import (
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"
)

// ValidatePocketIdEnvironment admits application settings supported by the pinned
// Pocket ID version. Listener, storage, identity, telemetry and file-loader
// controls remain managed by Borq. Diagnostics never include secret values.
func ValidatePocketIdEnvironment(environment map[string]string) error {
	encoded, err := json.Marshal(environment)
	if err != nil || len(encoded) > 128*1024 {
		return errors.New("Pocket ID environment must contain at most 128 KiB")
	}
	for name, value := range environment {
		if !pocketIdEnvironmentAllowed(name) {
			return errors.New("Pocket ID environment contains an unsupported or managed setting")
		}
		if !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
			return errors.New("Pocket ID environment values must be valid text without NUL bytes")
		}
	}
	return nil
}

func pocketIdEnvironmentAllowed(name string) bool {
	// Keep exact v2.19.1 names: a prefix rule would also admit *_FILE aliases and
	// future runtime controls. The legacy email names still have upstream migration support.
	switch name {
	case "APP_NAME", "SESSION_DURATION", "HOME_PAGE_URL", "ACCENT_COLOR", "DISABLE_ANIMATIONS", "ALLOW_OWN_ACCOUNT_EDIT",
		"REQUIRE_USER_EMAIL", "EMAILS_VERIFIED", "ALLOW_USER_SIGNUPS", "SIGNUP_DEFAULT_USER_GROUP_IDS", "SIGNUP_DEFAULT_CUSTOM_CLAIMS",
		"WEBAUTHN_USER_VERIFICATION", "WEBAUTHN_ALLOW_SYNCED_PASSKEYS", "WEBAUTHN_AUTHENTICATOR_ATTACHMENT",
		"SMTP_HOST", "SMTP_PORT", "SMTP_FROM", "SMTP_USER", "SMTP_PASSWORD", "SMTP_TLS", "SMTP_SKIP_CERT_VERIFY",
		"EMAIL_LOGIN_NOTIFICATION_MODE", "EMAIL_ONE_TIME_ACCESS_AS_ADMIN_ENABLED", "EMAIL_ONE_TIME_ACCESS_AS_UNAUTHENTICATED_ENABLED",
		"EMAIL_API_KEY_EXPIRATION_ENABLED", "EMAIL_VERIFICATION", "EMAIL_LOGIN_NOTIFICATION_ENABLED", "EMAIL_VERIFICATION_ENABLED",
		"LDAP_ENABLED", "LDAP_URL", "LDAP_BIND_DN", "LDAP_BIND_PASSWORD", "LDAP_BASE", "LDAP_USER_SEARCH_FILTER", "LDAP_USER_GROUP_SEARCH_FILTER",
		"LDAP_SKIP_CERT_VERIFY", "LDAP_SOFT_DELETE_USERS", "LDAP_ATTRIBUTE_USER_UNIQUE_IDENTIFIER", "LDAP_ATTRIBUTE_USER_USERNAME",
		"LDAP_ATTRIBUTE_USER_EMAIL", "LDAP_ATTRIBUTE_USER_FIRST_NAME", "LDAP_ATTRIBUTE_USER_LAST_NAME", "LDAP_ATTRIBUTE_USER_DISPLAY_NAME",
		"LDAP_ATTRIBUTE_USER_PROFILE_PICTURE", "LDAP_ATTRIBUTE_GROUP_MEMBER", "LDAP_ATTRIBUTE_GROUP_UNIQUE_IDENTIFIER", "LDAP_ATTRIBUTE_GROUP_NAME",
		"LDAP_ADMIN_GROUP_NAME", "CIMD_URL_ALLOWLIST":
		return true
	}
	return false
}
