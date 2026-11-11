package appapi

import (
	"context"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/business/server/apps"
)

// TestApiPermissionContract owns coverage of every documented operation and rejects
// contract shapes that could turn a newly added endpoint into an authorization bypass.
func TestApiPermissionContract(t *testing.T) {
	assert := is.New(t)
	specification, err := GetSwagger()
	assert.NoErr(err)
	assert.NoErr(specification.Validate(context.Background()))
	for _, item := range specification.Paths.Map() {
		for _, operation := range item.Operations() {
			t.Run(operation.OperationID, func(t *testing.T) {
				assert := is.New(t)
				security, err := operationSecurity(specification, operation)
				assert.NoErr(err)
				assert.True(apps.ValidPermission(security.permission))
				assert.True(operation.Responses.Status(401) != nil)
				assert.True(operation.Responses.Status(403) != nil)
			})
		}
	}
	for _, test := range []struct {
		name         string
		requirements *openapi3.SecurityRequirements
	}{
		{"missing", nil},
		{"empty", &openapi3.SecurityRequirements{}},
		{"anonymous", &openapi3.SecurityRequirements{{}}},
		{"unknown", &openapi3.SecurityRequirements{{"Unregistered": {}}}},
		{"bearer scopes", &openapi3.SecurityRequirements{{"AppRead": {"app:read"}}}},
		{"alternative grants", &openapi3.SecurityRequirements{{"AppRead": {}}, {"AppDeploy": {}}}},
		{"ambiguous grant", &openapi3.SecurityRequirements{{"AppRead": {}, "AppDeploy": {}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := operationSecurity(specification, &openapi3.Operation{OperationID: "NewOperation", Security: test.requirements})
			is.New(t).True(err != nil)
		})
	}
}

// TestApiPermissionSchemeValidation rejects unknown business grants, malformed
// conditional scope declarations and schemes that misrepresent bearer authentication.
func TestApiPermissionSchemeValidation(t *testing.T) {
	for _, test := range []struct{ name, kind, scheme, permission string }{
		{"unknown permission", "http", "bearer", "app:unknown"},
		{"wildcard", "http", "bearer", "*"},
		{"wrong scheme", "http", "basic", "app:read"},
		{"wrong type", "apiKey", "bearer", "app:read"},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			specification, err := GetSwagger()
			assert.NoErr(err)
			specification.Components.SecuritySchemes["AppRead"].Value.Type = test.kind
			specification.Components.SecuritySchemes["AppRead"].Value.Scheme = test.scheme
			specification.Components.SecuritySchemes["AppRead"].Value.Extensions["x-borq-permission"] = test.permission
			_, err = operationSecurity(specification, &openapi3.Operation{OperationID: "NewOperation", Security: &openapi3.SecurityRequirements{{"AppRead": {}}}})
			assert.True(err != nil)
		})
	}
	assert := is.New(t)
	specification, err := GetSwagger()
	assert.NoErr(err)
	operation := specification.Paths.Value("/api/v1/management/component-catalog").Get
	security, err := operationSecurity(specification, operation)
	assert.NoErr(err)
	assert.Equal(security.permission, apps.PermissionManagementRead)
	assert.Equal(security.targetPermission, apps.PermissionClusterRead)
	operation.Extensions["x-borq-target-security"] = map[string]any{"query": "scope", "value": "target", "organizationQuery": "organization", "scheme": "AppRead"}
	_, err = operationSecurity(specification, operation)
	assert.True(err != nil)
}
