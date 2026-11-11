package cloudflare

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/matryer/is"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCloudflareDnsCleanup owns provider receipt/target checks, deletion ordering and retry after a partial failure.
func TestCloudflareDnsCleanup(t *testing.T) {
	for _, mode := range []string{"owned", "competing receipt", "changed target", "partial failure"} {
		t.Run(mode, func(t *testing.T) {
			assert := is.New(t)
			receiptID, targetID, foreignID := strings.Repeat("1", 32), strings.Repeat("2", 32), strings.Repeat("3", 32)
			target := "11111111-1111-4111-8111-111111111111.cfargotunnel.com"
			records := map[string]dnsRecord{
				receiptID: {ID: receiptID, Type: "TXT", Name: "_borq.cname-production.example.test", Content: "heritage=external-dns,external-dns/owner=borq-physical,external-dns/resource=crd/borq-mgmt/owned"},
				targetID:  {ID: targetID, Type: "CNAME", Name: "production.example.test", Content: target},
				foreignID: {ID: foreignID, Type: "TXT", Name: "_borq.cname-unrelated.example.test", Content: "heritage=external-dns,external-dns/owner=foreign,external-dns/resource=crd/other/foreign"},
			}
			if mode == "competing receipt" {
				record := records[foreignID]
				record.Name = records[receiptID].Name
				records[foreignID] = record
			}
			if mode == "changed target" {
				record := records[targetID]
				record.Content = "foreign.example.test"
				records[targetID] = record
			}
			var deleted []string
			denyReceipt := mode == "partial failure"
			base := "/client/v4/zones/" + strings.Repeat("b", 32)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				if request.URL.Path == base {
					_ = json.NewEncoder(writer).Encode(map[string]any{"success": true, "result": map[string]string{"id": strings.Repeat("b", 32), "name": "example.test"}})
					return
				}
				if request.URL.Path == base+"/dns_records" {
					entries := []dnsRecord{}
					for _, record := range records {
						if record.Type != request.URL.Query().Get("type") {
							continue
						}
						if name := request.URL.Query().Get("name"); name != "" && name != record.Name {
							continue
						}
						if prefix := request.URL.Query().Get("name.startswith"); prefix != "" && !strings.HasPrefix(record.Name, prefix) {
							continue
						}
						entries = append(entries, record)
					}
					_ = json.NewEncoder(writer).Encode(map[string]any{"success": true, "result": entries, "result_info": map[string]int{"page": 1, "per_page": 100, "count": len(entries), "total_pages": 1}})
					return
				}
				id := strings.TrimPrefix(request.URL.Path, base+"/dns_records/")
				record, exists := records[id]
				if !exists {
					writer.WriteHeader(http.StatusNotFound)
					return
				}
				if request.Method == http.MethodDelete {
					if id == receiptID && denyReceipt {
						writer.WriteHeader(http.StatusForbidden)
						return
					}
					deleted = append(deleted, id)
					delete(records, id)
				}
				_ = json.NewEncoder(writer).Encode(map[string]any{"success": true, "result": record})
			}))
			defer server.Close()
			client := testClient(t, server.URL)
			input := DnsCleanupInput{OwnerID: "borq-physical", Namespace: "borq-mgmt", Domain: "example.test", Targets: []string{target}}
			plan, err := client.CleanupExternalDns(t.Context(), input)
			if mode == "competing receipt" || mode == "changed target" {
				assert.True(err != nil)
				assert.Equal(len(deleted), 0)
				input.Confirm = true
				_, err = client.CleanupExternalDns(t.Context(), input)
				assert.True(err != nil)
				assert.Equal(len(deleted), 0)
				assert.Equal(len(records), 3)
				return
			}
			assert.NoErr(err)
			assert.Equal(len(plan), 2)
			assert.Equal(len(deleted), 0)
			input.Confirm = true
			_, err = client.CleanupExternalDns(t.Context(), input)
			if mode == "partial failure" {
				assert.True(err != nil)
				assert.Equal(deleted, []string{targetID})
				_, exists := records[receiptID]
				assert.True(exists)
				denyReceipt = false
				_, err = client.CleanupExternalDns(t.Context(), input)
			}
			assert.NoErr(err)
			assert.Equal(deleted, []string{targetID, receiptID})
			assert.Equal(len(records), 1)
			plan, err = client.CleanupExternalDns(t.Context(), input)
			assert.NoErr(err)
			assert.Equal(len(plan), 0)
		})
	}
}

// TestCloudflareTunnelCleanup proves exact stored identity/configuration checks and idempotent provider deletion without creating a tunnel.
func TestCloudflareTunnelCleanup(t *testing.T) {
	for _, mode := range []string{"owned", "disabled provider defaults", "private routing", "foreign route", "foreign identity"} {
		t.Run(mode, func(t *testing.T) {
			assert := is.New(t)
			owner, namespace, domain := "borq-physical", "borq-mgmt", "example.test"
			digest := sha256.Sum256([]byte(owner + "/" + namespace + "/" + strings.Repeat("b", 32)))
			tunnel := Tunnel{ID: "11111111-1111-4111-8111-111111111111", AccountID: strings.Repeat("a", 32), Name: "borq-" + hex.EncodeToString(digest[:16])}
			var deleted []string
			gone := false
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				var value any
				switch {
				case strings.Contains(request.URL.Path, "/zones/"):
					value = map[string]any{"id": strings.Repeat("b", 32), "account": map[string]string{"id": tunnel.AccountID}}
				case strings.HasSuffix(request.URL.Path, "/configurations"):
					configuration := map[string]any{"ingress": []any{map[string]string{"hostname": "*.example.test", "service": "http://borq-traefik.borq-system.svc.cluster.local:80"}, map[string]string{"service": "http_status:404"}}}
					if mode == "disabled provider defaults" {
						configuration["warp-routing"] = map[string]bool{"enabled": false}
						configuration["originRequest"] = map[string]any{}
					}
					if mode == "private routing" {
						configuration["warp-routing"] = map[string]bool{"enabled": true}
					}
					if mode == "foreign route" {
						configuration["ingress"] = []any{map[string]string{"hostname": "foreign.example.test", "service": "http://foreign"}}
					}
					value = map[string]any{"config": configuration, "version": 2, "source": "cloudflare"}
				default:
					if request.Method == http.MethodDelete {
						deleted = append(deleted, request.URL.Path)
						if !strings.HasSuffix(request.URL.Path, "/connections") {
							gone = true
						}
						value = map[string]any{}
					} else {
						name := tunnel.Name
						if mode == "foreign identity" {
							name = "foreign"
						}
						record := map[string]any{"id": tunnel.ID, "name": name, "config_src": "cloudflare"}
						if gone {
							record["deleted_at"] = "2026-11-03T00:00:00Z"
						}
						value = record
					}
				}
				_ = json.NewEncoder(writer).Encode(map[string]any{"success": true, "result": value})
			}))
			defer server.Close()
			client := testClient(t, server.URL)
			err := client.CleanupTunnel(t.Context(), tunnel, owner, namespace, domain, false)
			if mode == "private routing" || mode == "foreign route" || mode == "foreign identity" {
				assert.True(err != nil)
				assert.Equal(len(deleted), 0)
				assert.True(client.CleanupTunnel(t.Context(), tunnel, owner, namespace, domain, true) != nil)
				assert.Equal(len(deleted), 0)
				return
			}
			assert.NoErr(err)
			assert.Equal(len(deleted), 0)
			assert.NoErr(client.CleanupTunnel(t.Context(), tunnel, owner, namespace, domain, true))
			assert.Equal(len(deleted), 2)
			assert.True(strings.HasSuffix(deleted[0], "/connections"))
			assert.True(strings.HasSuffix(deleted[1], "/"+tunnel.ID))
			assert.NoErr(client.CleanupTunnel(t.Context(), tunnel, owner, namespace, domain, true))
			assert.Equal(len(deleted), 2)
		})
	}
}
