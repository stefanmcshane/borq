package appserver

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/matryer/is"
)

// TestPocketIdEnrollmentBoundary ensures public GETs and foreign browser POSTs cannot mint admin links.
func TestPocketIdEnrollmentBoundary(t *testing.T) {
	assert := is.New(t)
	directory := t.TempDir()
	server, err := New(t.Context(), NewInput{Port: 7833, DataDir: directory})
	assert.NoErr(err)
	defer server.Close()
	address := "https://borq.test/api/auth/pocketid/admin/team"
	request := httptest.NewRequest(http.MethodPost, address, nil)
	request.Header.Set("Origin", "https://foreign.test")
	authorizeAdminTestRequest(t, directory, request)
	response := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(response, request)
	assert.Equal(response.Code, http.StatusForbidden)
	assert.Equal(response.Header().Get("Location"), "")
	request = httptest.NewRequest(http.MethodGet, address, nil)
	response = httptest.NewRecorder()
	server.http.Handler.ServeHTTP(response, request)
	assert.True(response.Code >= 400)
	assert.Equal(response.Header().Get("Location"), "")
	request = httptest.NewRequest(http.MethodPost, address, nil)
	authorizeAdminTestRequest(t, directory, request)
	response = httptest.NewRecorder()
	server.http.Handler.ServeHTTP(response, request)
	assert.Equal(response.Code, http.StatusBadRequest)
	assert.Equal(response.Header().Get("Cache-Control"), "no-store")
	assert.Equal(response.Header().Get("Referrer-Policy"), "no-referrer")
	assert.True(strings.Contains(response.Body.String(), "unavailable for this organization"))
}
