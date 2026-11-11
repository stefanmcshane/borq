package appserver

import (
	"net/http"
	"net/url"

	"github.com/stefanmcshane/borq/business/server/apps"
)

func pocketIdEnrollment(manager apps.Manager) http.Handler {
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if err := manager.Authorize(request.Context(), apps.PermissionManagementConfigure, request.PathValue("organization")); err != nil {
			authProblem(writer, http.StatusForbidden, "Permission denied.")
			return
		}
		writer.Header().Set("Cache-Control", "no-store")
		writer.Header().Set("Referrer-Policy", "no-referrer")
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		settings, err := manager.GetOrganizationPocketId(request.Context(), request.PathValue("organization"))
		if err != nil {
			http.Error(writer, "Managed Pocket ID is unavailable for this organization.", http.StatusBadRequest)
			return
		}
		address, err := url.Parse(settings.BaseURL)
		if err != nil || address.Host == "" || address.Host != request.Host {
			http.Error(writer, "Open authentication settings from the saved Borq address before enrolling the administrator.", http.StatusBadRequest)
			return
		}
		location, err := manager.PocketIdAdminEnrollment(request.Context(), request.PathValue("organization"))
		if err != nil {
			message := apps.OrganizationOidcRequestMessage(err)
			if message == "" {
				message = "Could not start administrator enrollment. Check Pocket ID setup and retry."
			}
			http.Error(writer, message, http.StatusBadRequest)
			return
		}
		// Only this explicit POST carries a one-time enrollment credential to the browser.
		http.Redirect(writer, request, location, http.StatusSeeOther)
	})
	return http.NewCrossOriginProtection().Handler(handler)
}
