package managementbackup

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/matryer/is"
)

// TestStaticVaultRecoveryDeferred refuses unsupported static-seal restore before
// touching a recovery instance or attempting the older Shamir initialization.
func TestStaticVaultRecoveryDeferred(t *testing.T) {
	assert := is.New(t)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		writer.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	err := restoreOpenBao(t.Context(), OpenBaoRecovery{Address: server.URL, OriginalToken: "retained-token", SealKey: "retained-seal-key"}, "/must-not-read")
	assert.True(err != nil && strings.Contains(err.Error(), "not implemented yet"))
	assert.Equal(requests, 0)
}
