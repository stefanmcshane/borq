package appserver

import (
	"net/http"

	"github.com/stefanmcshane/borq/business/server/apps"
)

// Custom identity routes bypass generated API dispatch but must finish before
// teardown removes their provider. Recovery token login/logout remain available.
func managementApplicationAccess(manager apps.Manager, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		release, err := manager.BeginApplicationRequest(request.Context())
		if err != nil {
			http.Error(writer, "Management teardown is active; ordinary operations are paused.", http.StatusConflict)
			return
		}
		defer release()
		next.ServeHTTP(writer, request)
	})
}
