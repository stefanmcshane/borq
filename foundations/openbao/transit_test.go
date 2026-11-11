package openbao_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/openbao"
)

// TestTransitMountPreservation refuses to replace a different engine and makes repeat setup idempotent.
func TestTransitMountPreservation(t *testing.T) {
	for _, existing := range []string{"", "transit", "kv"} {
		t.Run(existing, func(t *testing.T) {
			check := is.New(t)
			created := false
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				if request.URL.Path == "/v1/sys/mounts" {
					mounts := map[string]any{}
					if existing != "" {
						mounts["borq-rustfs-kms/"] = map[string]any{"type": existing}
					}
					check.NoErr(json.NewEncoder(writer).Encode(map[string]any{"data": mounts}))
					return
				}
				check.Equal(request.URL.Path, "/v1/sys/mounts/borq-rustfs-kms")
				var input struct {
					Type string `json:"type"`
				}
				check.NoErr(json.NewDecoder(request.Body).Decode(&input))
				check.Equal(input.Type, "transit")
				created = true
				writer.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			client, err := openbao.NewClient(openbao.NewClientInput{Address: server.URL, Token: "explicit-test-token"})
			check.NoErr(err)
			err = client.EnsureTransit(t.Context(), "borq-rustfs-kms")
			if existing == "kv" {
				check.True(err != nil)
			} else {
				check.NoErr(err)
			}
			check.Equal(created, existing == "")
		})
	}
}
