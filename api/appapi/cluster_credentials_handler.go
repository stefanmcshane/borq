package appapi

import "net/http"

// GetClusterKubeconfig returns credentials only through this explicit download.
func (server handler) GetClusterKubeconfig(writer http.ResponseWriter, request *http.Request, organization, cluster string) {
	writer.Header().Set("Cache-Control", "no-store")
	result, err := server.manager.GetClusterKubeconfig(request.Context(), organization, cluster)
	respond[ClusterKubeconfig](writer, http.StatusOK, result, err)
}
