package appapi

import "net/http"

// GetClusterMetrics returns a live snapshot without exposing saved cluster credentials.
func (server handler) GetClusterMetrics(writer http.ResponseWriter, request *http.Request, organization, cluster string) {
	writer.Header().Set("Cache-Control", "no-store")
	result, err := server.manager.GetClusterMetrics(request.Context(), organization, cluster)
	respond[ClusterMetrics](writer, http.StatusOK, result, err)
}
