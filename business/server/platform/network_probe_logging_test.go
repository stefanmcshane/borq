package platform

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
)

// TestNetworkProbeLogging drives the actual curl probe against a local TLS
// server, proving failed checks explain their cause without logging the URL.
func TestNetworkProbeLogging(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl is required")
	}
	for _, test := range []struct {
		name, want string
		status     int
		trust      bool
	}{
		{"success", "Management HTTPS verification succeeded", 200, true},
		{"http failure", "unexpected HTTP status 503", 503, true},
		{"redirect", "unexpected HTTP status 302", 302, true},
		{"untrusted certificate", "TLS certificate verification failed", 200, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { writer.WriteHeader(test.status) }))
			defer server.Close()
			address, err := url.Parse(server.URL)
			assert.NoErr(err)
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, "/bin/sh", "-ec", networkProbeScript())
			command.WaitDelay = 100 * time.Millisecond
			command.Env = append(os.Environ(), "PROBE_URL="+server.URL+"/private-url-marker", "CONNECT_TO="+address.Host+":"+address.Host, "EXPECTED_STATUS=200")
			if test.trust {
				certificate := filepath.Join(t.TempDir(), "ca.pem")
				assert.NoErr(os.WriteFile(certificate, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600))
				command.Env = append(command.Env, "CURL_CA_BUNDLE="+certificate)
			}
			output, err := command.CombinedOutput()
			if test.status == 200 && test.trust {
				assert.NoErr(err)
			}
			assert.True(strings.Contains(string(output), test.want))
			assert.True(!strings.Contains(string(output), "private-url-marker"))
		})
	}
}
