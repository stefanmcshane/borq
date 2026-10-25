package appapi

import (
	"github.com/stefanmcshane/borq/business/server/apps"
	"github.com/stefanmcshane/borq/business/server/platform"
	"net/http"
)

// GetComponentCatalog returns desired Helm input without connecting to an installation.
func (server handler) GetComponentCatalog(writer http.ResponseWriter, request *http.Request, params GetComponentCatalogParams) {
	writer.Header().Set("Cache-Control", "no-store")
	scope := platform.ComponentScopeManagement
	if params.Scope != nil {
		scope = platform.ComponentScope(*params.Scope)
	}
	organization, cluster := "", ""
	if params.Organization != nil {
		organization = *params.Organization
	}
	if params.Cluster != nil {
		cluster = *params.Cluster
	}
	result, err := server.manager.GetComponentCatalog(request.Context(), scope, organization, cluster)
	respond[ComponentCatalog](writer, http.StatusOK, result, err)
}

// GetManagementConfiguration exports accepted configuration with credential references.
func (server handler) GetManagementConfiguration(writer http.ResponseWriter, request *http.Request) {
	result, err := server.manager.GetManagementConfiguration(request.Context())
	respond[ManagementConfiguration](writer, http.StatusOK, result, err)
}

// ApplyManagementConfiguration accepts the unified document through the durable manager.
func (server handler) ApplyManagementConfiguration(writer http.ResponseWriter, request *http.Request) {
	var body ManagementConfiguration
	if !server.decodeBody(writer, request, "ManagementConfiguration", &body) {
		return
	}
	input, err := convert[apps.ManagementConfiguration](body)
	if err != nil {
		writeProblem(writer, http.StatusBadRequest, "invalid management configuration")
		return
	}
	result, err := server.manager.ApplyManagementConfiguration(request.Context(), input)
	respond[Operation](writer, http.StatusAccepted, result, err)
}

// ApplySpaceConfiguration reuses the same named-space model as a management document.
func (server handler) ApplySpaceConfiguration(writer http.ResponseWriter, request *http.Request, organization OrganizationPath, space SpacePath) {
	var body SpaceConfiguration
	if !server.decodeBody(writer, request, "SpaceConfiguration", &body) {
		return
	}
	input, err := convert[apps.SpaceConfiguration](body)
	if err != nil {
		writeProblem(writer, http.StatusBadRequest, "invalid space configuration")
		return
	}
	result, err := server.manager.ApplyManagementConfiguration(request.Context(), apps.ManagementConfiguration{Organizations: []apps.ManagementOrganization{{Name: organization, Spaces: []apps.ManagementSpace{{Name: space, SpaceConfiguration: input}}}}})
	respond[Operation](writer, http.StatusAccepted, result, err)
}

// GetManagementOperation reads durable aggregate apply progress.
func (server handler) GetManagementOperation(writer http.ResponseWriter, request *http.Request, operation string) {
	result, err := server.manager.GetManagementOperation(request.Context(), operation)
	respond[Operation](writer, http.StatusOK, result, err)
}

// GetManagementS3 returns shared storage coordinates with a safe credential reference.
func (server handler) GetManagementS3(writer http.ResponseWriter, request *http.Request) {
	result, err := server.manager.GetManagementS3(request.Context())
	respond[ManagementS3](writer, http.StatusOK, result, err)
}

// PutManagementS3 validates replacement credentials before accepting storage changes.
func (server handler) PutManagementS3(writer http.ResponseWriter, request *http.Request) {
	var body ManagementS3Input
	if !server.decodeBody(writer, request, "ManagementS3Input", &body) {
		return
	}
	input, err := convert[apps.ManagementS3Input](body)
	if err != nil {
		writeProblem(writer, http.StatusBadRequest, "invalid storage configuration")
		return
	}
	result, err := server.manager.PutManagementS3(request.Context(), input)
	respond[ManagementS3](writer, http.StatusOK, result, err)
}

// DeleteManagementS3 delegates dependency guards to the management owner.
func (server handler) DeleteManagementS3(writer http.ResponseWriter, request *http.Request) {
	if !emptyBody(writer, request) {
		return
	}
	err := server.manager.DeleteManagementS3(request.Context())
	respond[ManagementS3](writer, http.StatusOK, ManagementS3{}, err)
}

// SetOrganizationManagedAddons explicitly grants an organization use of shared storage.
func (server handler) SetOrganizationManagedAddons(writer http.ResponseWriter, request *http.Request, organization string) {
	var body OrganizationManagedAddonsInput
	if !server.decodeBody(writer, request, "OrganizationManagedAddonsInput", &body) {
		return
	}
	result, err := server.manager.SetOrganizationManagedAddons(request.Context(), organization, body.Enabled)
	respond[Organization](writer, http.StatusOK, result, err)
}
