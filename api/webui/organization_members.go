package webui

import (
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"

	"github.com/stefanmcshane/borq/api/appapi"
	"github.com/stefanmcshane/borq/business/server/apps"
)

func organizationUiPermission(principal apps.Principal, permission apps.Permission) bool {
	return slices.Contains(principal.Permissions, permission) || slices.Contains(principal.Permissions, apps.Permission("*"))
}

func (web handler) organizationMemberPermissions() []string {
	var permissions []string
	schema := web.inputSchemas["Permission"].Value
	if schema == nil {
		return permissions
	}
	for _, value := range schema.Enum {
		permission, ok := value.(string)
		if !ok || permission == "management:read" || permission == "management:configure" || permission == "organization:create" {
			continue
		}
		permissions = append(permissions, permission)
	}
	sort.Strings(permissions)
	return permissions
}

func (web handler) executeOrganizationMember(request *http.Request, result actionResult) (actionResult, error) {
	form := request.PostForm
	result.selection.Set("view", "organization-members")
	result.selection.Del("space")
	path := organizationPath(form.Get("organization"))
	if form.Get("action") == "put-organization-member" {
		revision, err := strconv.ParseInt(form.Get("revision"), 10, 64)
		if err != nil || revision < 1 {
			return result, apiProblem{status: http.StatusBadRequest, message: "Reload Members before saving this person's roles."}
		}
		input := appapi.OrganizationMemberInput{RoleIds: append([]string{}, form["roleIds"]...), Disabled: form.Get("disabled") == "true", Revision: revision}
		if _, err := requestApi[appapi.OrganizationMember](request.Context(), web.api, http.MethodPut, path+"/members/"+url.PathEscape(form.Get("member")), input); err != nil {
			return result, err
		}
		result.notice = "Member roles updated. Changes apply to existing sessions; previously issued API tokens are revoked."
		return result, nil
	}
	path += "/roles"
	permissions := make([]appapi.Permission, 0, len(form["permissions"]))
	for _, permission := range form["permissions"] {
		permissions = append(permissions, appapi.Permission(permission))
	}
	input := appapi.OrganizationRoleInput{Name: form.Get("name"), Permissions: permissions}
	method := http.MethodPost
	if form.Get("action") != "create-organization-role" {
		revision, err := strconv.ParseInt(form.Get("revision"), 10, 64)
		if err != nil || revision < 1 {
			return result, apiProblem{status: http.StatusBadRequest, message: "Reload Members before changing this role."}
		}
		input.Revision = revision
		method = http.MethodPut
		path += "/" + url.PathEscape(form.Get("role"))
	}
	if form.Get("action") == "delete-organization-role" {
		if _, err := requestApi[struct{}](request.Context(), web.api, http.MethodDelete, path+"?revision="+strconv.FormatInt(input.Revision, 10), nil); err != nil {
			return result, err
		}
		result.notice = "Role deleted."
		return result, nil
	}
	if _, err := requestApi[appapi.OrganizationRole](request.Context(), web.api, method, path, input); err != nil {
		return result, err
	}
	result.notice = "Role saved. Permission changes apply to existing sessions and API tokens."
	return result, nil
}
