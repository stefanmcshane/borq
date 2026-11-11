package cloudflare

import (
	"encoding/json"
	"fmt"
	"github.com/matryer/is"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestManagedTunnelProtocol proves account derivation, create retry recovery,
// full configuration updates, idempotent repeats and separate run-token retrieval.
func TestManagedTunnelProtocol(t *testing.T) {
	is := is.New(t)
	account := strings.Repeat("a", 32)
	id := "11111111-1111-4111-8111-111111111111"
	name := "borq-managed-test"
	created, updates := 0, 0
	exists := false
	current := map[string]any{"config": map[string]any{"ingress": []any{map[string]any{"service": "http_status:404"}}}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		is.Equal(r.Header.Get("Authorization"), "Bearer test-private-token")
		var result any
		base := "/client/v4/accounts/" + account + "/cfd_tunnel"
		switch {
		case r.URL.Path == "/client/v4/zones/"+strings.Repeat("b", 32):
			result = map[string]any{"id": strings.Repeat("b", 32), "account": map[string]any{"id": account}}
		case r.URL.Path == base && r.Method == http.MethodGet:
			is.Equal(r.URL.Query().Get("name"), name)
			result = []any{}
			if exists {
				result = []any{map[string]any{"id": id, "name": name, "config_src": "cloudflare"}}
			}
		case r.URL.Path == base && r.Method == http.MethodPost:
			var body map[string]any
			is.NoErr(json.NewDecoder(r.Body).Decode(&body))
			is.Equal(body, map[string]any{"name": name, "config_src": "cloudflare"})
			created++
			exists = true
			result = map[string]any{"id": id, "name": name, "config_src": "cloudflare"}
		case r.URL.Path == base+"/"+id:
			result = map[string]any{"id": id, "name": name, "config_src": "cloudflare"}
		case r.URL.Path == base+"/"+id+"/configurations":
			if r.Method == http.MethodPut {
				updates++
				is.NoErr(json.NewDecoder(r.Body).Decode(&current))
			}
			result = current
		case r.URL.Path == base+"/"+id+"/token":
			result = "private-connector-run-token"
		default:
			t.Errorf("unexpected tunnel request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
			return
		}
		is.NoErr(json.NewEncoder(w).Encode(map[string]any{"success": true, "result": result}))
	}))
	defer server.Close()
	client := testClient(t, server.URL)
	first, err := client.EnsureTunnel(t.Context(), name, "")
	is.NoErr(err)
	recovered, err := client.EnsureTunnel(t.Context(), name, "")
	is.NoErr(err)
	is.Equal(recovered.ID, first.ID)
	stored, err := client.EnsureTunnel(t.Context(), name, id)
	is.NoErr(err)
	is.Equal(stored, first)
	is.Equal(created, 1)
	is.NoErr(client.ConfigureTunnel(t.Context(), stored, "example.com"))
	is.NoErr(client.ConfigureTunnel(t.Context(), stored, "example.com"))
	is.Equal(updates, 1)
	config := current["config"].(map[string]any)
	routes := config["ingress"].([]any)
	is.Equal(len(routes), 2)
	route := routes[0].(map[string]any)
	is.Equal(route["hostname"], "*.example.com")
	is.Equal(route["service"], "http://borq-traefik.borq-system.svc.cluster.local:80")
	is.True(route["originRequest"] == nil)
	token, err := client.TunnelToken(t.Context(), stored)
	is.NoErr(err)
	is.Equal(token, "private-connector-run-token")
	encoded, err := json.Marshal(stored)
	is.NoErr(err)
	is.True(!strings.Contains(string(encoded), token))
}

// TestTunnelOwnershipAndErrorRedaction refuses an unrelated remote tunnel and
// ensures provider error bodies cannot leak either API or connector credentials.
func TestTunnelOwnershipAndErrorRedaction(t *testing.T) {
	for _, scenario := range []string{"foreign name", "local source", "deleted", "forbidden", "invalid account"} {
		t.Run(scenario, func(t *testing.T) {
			is := is.New(t)
			id := "11111111-1111-4111-8111-111111111111"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Fatal("guard attempted a write")
				}
				if strings.Contains(r.URL.Path, "/zones/") {
					account := strings.Repeat("a", 32)
					if scenario == "invalid account" {
						account = "../escape"
					}
					fmt.Fprintf(w, `{"success":true,"result":{"id":%q,"account":{"id":%q}}}`, strings.Repeat("b", 32), account)
					return
				}
				if scenario == "forbidden" {
					w.WriteHeader(403)
					fmt.Fprint(w, "test-private-token secret-run-token")
					return
				}
				name, source := "borq-test", "cloudflare"
				var deleted any
				switch scenario {
				case "foreign name":
					name = "other-installation"
				case "local source":
					source = "local"
				case "deleted":
					deleted = "2026-01-01T00:00:00Z"
				}
				is.NoErr(json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]any{"id": id, "name": name, "config_src": source, "deleted_at": deleted}}))
			}))
			defer server.Close()
			_, err := testClient(t, server.URL).EnsureTunnel(t.Context(), "borq-test", id)
			is.True(err != nil)
			is.True(!strings.Contains(err.Error(), "test-private-token"))
			is.True(!strings.Contains(err.Error(), "secret-run-token"))
		})
	}
}
