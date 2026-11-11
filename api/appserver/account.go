package appserver

import (
	"net/http"
	"slices"
	"strings"

	"github.com/stefanmcshane/borq/business/server/apps"
)

func (auth *authentication) account(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.Header().Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	principal, _ := apps.PrincipalFromContext(request.Context())
	permissions := principal.Permissions
	if slices.Contains(permissions, apps.Permission("*")) {
		permissions = []apps.Permission{apps.PermissionOrganizationRead, apps.PermissionClusterRead, apps.PermissionSpaceRead, apps.PermissionAppRead}
	}
	defaults := make([]string, 0, len(permissions))
	for _, permission := range permissions {
		if apps.ValidPermission(permission) && permission != apps.PermissionManagementRead && permission != apps.PermissionManagementConfigure && permission != apps.PermissionOrganizationCreate {
			defaults = append(defaults, string(permission))
		}
	}
	canCreate := !principal.Admin && (slices.Contains(principal.Permissions, apps.Permission("*")) || slices.Contains(principal.Permissions, apps.PermissionTokenCreate))
	_ = auth.page.ExecuteTemplate(writer, "account", struct {
		CanCreate   bool
		Permissions string
	}{canCreate, strings.Join(defaults, ",")})
}

func (auth *authentication) downloadToken(writer http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(writer, request.Body, 4096)
	if request.ParseForm() != nil {
		authProblem(writer, http.StatusBadRequest, "Invalid token form.")
		return
	}
	input := apps.CreateApiTokenInput{ExpiresInSeconds: 3600}
	for _, value := range strings.Split(request.PostForm.Get("permissions"), ",") {
		input.Permissions = append(input.Permissions, apps.Permission(strings.TrimSpace(value)))
	}
	token, err := auth.manager.CreateApiToken(request.Context(), input)
	if err != nil {
		authProblem(writer, http.StatusForbidden, "Sign in with your organization account and request only permissions you hold.")
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
	writer.Header().Set("Content-Disposition", `attachment; filename="borq-user.token"`)
	_, _ = writer.Write([]byte(token.Token + "\n"))
}

const accountPage = `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Account · Borq</title><link rel="stylesheet" href="/assets/style.css"></head><body><main class="card"><h1>Account</h1>{{if .CanCreate}}<p>Download a user token for the CLI or MCP. Tokens expire within one hour and grant only the permissions you choose.</p><form method="post" action="/account/tokens"><label>Permissions, separated by commas<input class="input" name="permissions" value="{{.Permissions}}" required></label><button class="btn" type="submit">Download user token</button></form><p>Use the downloaded file with <code>borq login --api-url URL --token-file PATH</code>.</p>{{else}}<p>User tokens require an organization account with the token:create permission. Ask your organization administrator for access.</p>{{end}}<form method="post" action="/api/auth/logout"><button class="btn" type="submit">Sign out</button></form><p><a href="/">Return to Borq</a></p></main></body></html>`
