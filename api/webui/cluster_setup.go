package webui

import (
	"net/http"

	"github.com/stefanmcshane/borq/api/appapi"
)

type clusterSetupData struct {
	// Setup describes the server's pinned components and configured namespace.
	Setup appapi.ClusterSetup
	// Problem keeps a failed catalog read visible within the setup disclosure.
	Problem string
}

func (web handler) clusterSetup(writer http.ResponseWriter, request *http.Request) {
	setup, err := requestApi[appapi.ClusterSetup](request.Context(), web.api, http.MethodGet, "/api/v1/cluster-setup", nil)
	data := clusterSetupData{Setup: setup}
	if err != nil {
		data.Problem = "Required component details could not be loaded. Try again."
	}
	web.respond(writer, request, "cluster-setup", data, http.StatusOK)
}
