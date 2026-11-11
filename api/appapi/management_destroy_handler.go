package appapi

import "net/http"

// PlanManagementDestroy reads the server's ownership-checked detach plan.
func (server handler) PlanManagementDestroy(writer http.ResponseWriter, request *http.Request) {
	result, err := server.manager.PlanManagementDestroy(request.Context())
	respond[ManagementDestroyPlan](writer, http.StatusOK, result, err)
}

// DestroyManagement queues durable teardown while retaining the private API.
func (server handler) DestroyManagement(writer http.ResponseWriter, request *http.Request) {
	if !emptyBody(writer, request) {
		return
	}
	result, err := server.manager.DestroyManagement(request.Context())
	respond[Operation](writer, http.StatusAccepted, result, err)
}

// GetManagementDestroy reads the durable operation and its immutable plan.
func (server handler) GetManagementDestroy(writer http.ResponseWriter, request *http.Request, operation string) {
	result, err := server.manager.GetManagementDestroy(request.Context(), operation)
	respond[ManagementDestroyStatus](writer, http.StatusOK, result, err)
}
