package apps

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/stefanmcshane/borq/foundations/sqlite"
)

type Permission string

const (
	PermissionOrganizationRead      Permission = "organization:read"
	PermissionOrganizationCreate    Permission = "organization:create"
	PermissionOrganizationConfigure Permission = "organization:configure"
	PermissionClusterRead           Permission = "cluster:read"
	PermissionClusterLink           Permission = "cluster:link"
	PermissionClusterConfigure      Permission = "cluster:configure"
	PermissionClusterCredentials    Permission = "cluster:credentials"
	PermissionSpaceRead             Permission = "space:read"
	PermissionSpaceConfigure        Permission = "space:configure"
	PermissionAppRead               Permission = "app:read"
	PermissionAppDeploy             Permission = "app:deploy"
	PermissionAppDelete             Permission = "app:delete"
	PermissionSecretRead            Permission = "secret:read"
	PermissionSecretWrite           Permission = "secret:write"
	PermissionDatabaseRead          Permission = "database:read"
	PermissionDatabaseConfigure     Permission = "database:configure"
	PermissionDatabaseRestore       Permission = "database:restore"
	PermissionDatabaseDelete        Permission = "database:delete"
	PermissionAddonRead             Permission = "addon:read"
	PermissionAddonConfigure        Permission = "addon:configure"
	PermissionAddonDelete           Permission = "addon:delete"
	PermissionManagementRead        Permission = "management:read"
	PermissionManagementConfigure   Permission = "management:configure"
	PermissionTokenCreate           Permission = "token:create"
	PermissionUserManage            Permission = "user:manage"
)

// Principal is supplied only by authenticated transport boundaries, never decoded
// from a request. Admin credentials are installation-wide; user grants are bound
// to an immutable organization and verified provider identity.
type Principal struct {
	Admin       bool
	Login       OrganizationLogin
	Permissions []Permission
	tokenKey    string
}

type principalContextKey struct{}

func WithPrincipal(ctx context.Context, principal Principal) context.Context {
	return context.WithValue(ctx, principalContextKey{}, principal)
}

func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	principal, ok := ctx.Value(principalContextKey{}).(Principal)
	return principal, ok
}

func ValidPermission(permission Permission) bool {
	switch permission {
	case PermissionOrganizationRead, PermissionOrganizationCreate, PermissionOrganizationConfigure,
		PermissionClusterRead, PermissionClusterLink, PermissionClusterConfigure, PermissionClusterCredentials,
		PermissionSpaceRead, PermissionSpaceConfigure, PermissionAppRead, PermissionAppDeploy, PermissionAppDelete,
		PermissionSecretRead, PermissionSecretWrite, PermissionDatabaseRead, PermissionDatabaseConfigure,
		PermissionDatabaseRestore, PermissionDatabaseDelete, PermissionAddonRead, PermissionAddonConfigure,
		PermissionAddonDelete, PermissionManagementRead, PermissionManagementConfigure, PermissionTokenCreate, PermissionUserManage:
		return true
	default:
		return false
	}
}

func (manager Manager) Authorize(ctx context.Context, permission Permission, organization string) error {
	principal, ok := PrincipalFromContext(ctx)
	if !ok {
		return ErrUnauthenticated
	}
	if !ValidPermission(permission) {
		return ErrForbidden
	}
	if principal.Admin {
		return nil
	}
	// Organization grants, including wildcard legacy grants, cannot confer
	// installation administration or authority to create another organization.
	switch permission {
	case PermissionManagementRead, PermissionManagementConfigure, PermissionOrganizationCreate:
		return ErrForbidden
	}
	if organization == "" || organization != principal.Login.Organization || manager.ValidateOrganizationLogin(ctx, principal.Login) != nil {
		return ErrForbidden
	}
	grants, err := manager.OrganizationLoginPermissions(ctx, principal.Login)
	if err != nil {
		return ErrForbidden
	}
	if !slices.Contains(grants, permission) && !slices.Contains(grants, Permission("*")) {
		return ErrForbidden
	}
	if principal.tokenKey != "" {
		token, _, err := loadDocument[apiTokenRecord](ctx, manager.database, "api-token", principal.tokenKey)
		if err != nil || !time.Now().Before(token.ExpiresAt) {
			return ErrForbidden
		}
		principal.Permissions = token.Permissions
	}
	if slices.Contains(principal.Permissions, permission) || slices.Contains(principal.Permissions, Permission("*")) {
		return nil
	}
	return ErrForbidden
}

// These errors distinguish absent credentials from an authenticated account
// that lacks a grant without exposing provider or secret-storage diagnostics.
var (
	ErrUnauthenticated = errors.New("authentication required")
	ErrForbidden       = errors.New("permission denied")
)

type apiTokenRecord struct {
	Login       OrganizationLogin `json:"login"`
	Permissions []Permission      `json:"permissions"`
	ExpiresAt   time.Time         `json:"expiresAt"`
}

type CreateApiTokenInput struct {
	Permissions      []Permission `json:"permissions"`
	ExpiresInSeconds int          `json:"expiresInSeconds"`
}

type ApiToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
}

func (manager Manager) CreateApiToken(ctx context.Context, input CreateApiTokenInput) (ApiToken, error) {
	manager.runtime.mutex.Lock()
	defer manager.runtime.mutex.Unlock()
	principal, ok := PrincipalFromContext(ctx)
	if !ok {
		return ApiToken{}, ErrUnauthenticated
	}
	if principal.Admin {
		return ApiToken{}, errors.New("sign in with an organization account to create a user token")
	}
	if err := manager.Authorize(ctx, PermissionTokenCreate, principal.Login.Organization); err != nil {
		return ApiToken{}, err
	}
	if len(input.Permissions) == 0 || len(input.Permissions) > 64 || input.ExpiresInSeconds < 1 || input.ExpiresInSeconds > 86400 {
		return ApiToken{}, ErrInvalid
	}
	for _, permission := range input.Permissions {
		if err := manager.Authorize(ctx, permission, principal.Login.Organization); err != nil {
			return ApiToken{}, err
		}
	}
	expires := time.Now().Add(time.Duration(input.ExpiresInSeconds) * time.Second)
	if expires.After(principal.Login.ExpiresAt) {
		expires = principal.Login.ExpiresAt
	}
	value := "borq_user_" + rand.Text() + rand.Text()
	digest := sha256.Sum256([]byte(value))
	stored := apiTokenRecord{Login: principal.Login, Permissions: slices.Clone(input.Permissions), ExpiresAt: expires}
	write, err := documentWrite("api-token", hex.EncodeToString(digest[:]), stored, 0)
	if err != nil {
		return ApiToken{}, err
	}
	if err := manager.database.Commit(ctx, []sqlite.Write{write}); err != nil {
		return ApiToken{}, err
	}
	return ApiToken{Token: value, ExpiresAt: expires}, nil
}

func (manager Manager) AuthenticateApiToken(ctx context.Context, token string) (Principal, error) {
	if !strings.HasPrefix(token, "borq_user_") || len(token) > 128 {
		return Principal{}, ErrUnauthenticated
	}
	digest := sha256.Sum256([]byte(token))
	stored, _, err := loadDocument[apiTokenRecord](ctx, manager.database, "api-token", hex.EncodeToString(digest[:]))
	if err != nil || !time.Now().Before(stored.ExpiresAt) || manager.ValidateOrganizationLogin(ctx, stored.Login) != nil {
		return Principal{}, ErrUnauthenticated
	}
	for _, permission := range stored.Permissions {
		if !ValidPermission(permission) {
			return Principal{}, ErrUnauthenticated
		}
	}
	// Delegation cannot extend the lifetime of the credential that authorized it.
	if stored.Login.ExpiresAt.After(stored.ExpiresAt) {
		stored.Login.ExpiresAt = stored.ExpiresAt
	}
	return Principal{Login: stored.Login, Permissions: slices.Clone(stored.Permissions), tokenKey: hex.EncodeToString(digest[:])}, nil
}
