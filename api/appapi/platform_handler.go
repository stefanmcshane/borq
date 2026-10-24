package appapi

import "net/http"

// GetManagementComponents returns the last management reconciliation state.
func (server handler) GetManagementComponents(writer http.ResponseWriter, request *http.Request) {
	result, err := server.manager.GetManagementComponents(request.Context())
	respond[ComponentInstallation](writer, http.StatusOK, result, err)
}

// UpgradeManagementComponents upgrades managed releases using the current Borq catalog.
func (server handler) UpgradeManagementComponents(writer http.ResponseWriter, request *http.Request) {
	if !emptyBody(writer, request) {
		return
	}
	result, err := server.manager.ReconcileManagementComponents(request.Context(), true)
	respond[ComponentInstallation](writer, http.StatusOK, result, err)
}

// GetClusterComponents returns the durable target run without scheduling work.
func (server handler) GetClusterComponents(writer http.ResponseWriter, request *http.Request, organization, cluster string) {
	result, err := server.manager.GetClusterComponents(request.Context(), organization, cluster)
	respond[ComponentInstallation](writer, http.StatusOK, result, err)
}

// DiffClusterComponents compares releases without changing or enqueueing anything.
func (server handler) DiffClusterComponents(writer http.ResponseWriter, request *http.Request, organization, cluster string, parameters DiffClusterComponentsParams) {
	upgrade := parameters.Upgrade != nil && *parameters.Upgrade
	result, err := server.manager.DiffClusterComponents(request.Context(), organization, cluster, upgrade)
	respond[ComponentDiff](writer, http.StatusOK, result, err)
}

// ReconcileClusterComponents queues missing or failed components and returns immediately.
func (server handler) ReconcileClusterComponents(writer http.ResponseWriter, request *http.Request, organization, cluster string) {
	if !emptyBody(writer, request) {
		return
	}
	result, err := server.manager.ReconcileClusterComponents(request.Context(), organization, cluster, false)
	respond[ComponentInstallation](writer, http.StatusOK, result, err)
}

// UpgradeClusterComponents queues an explicit upgrade using the pinned catalog.
func (server handler) UpgradeClusterComponents(writer http.ResponseWriter, request *http.Request, organization, cluster string) {
	if !emptyBody(writer, request) {
		return
	}
	result, err := server.manager.ReconcileClusterComponents(request.Context(), organization, cluster, true)
	respond[ComponentInstallation](writer, http.StatusOK, result, err)
}
