package cli

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/matryer/is"
)

// TestApplicationConnectionGuidance covers the user's dropped-connection failure
// across discovery and writes, without extra requests or automatic setup.
func TestApplicationConnectionGuidance(t *testing.T) {
	assert := is.New(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		connection, _, err := writer.(http.Hijacker).Hijack()
		assert.NoErr(err)
		assert.NoErr(connection.Close())
	}))
	defer server.Close()
	for _, arguments := range [][]string{
		{"org", "create", "testorg"},
		{"space", "list", "--organization", "team"},
		{"apply", "--plan", "saved-plan"},
	} {
		output, _, err := runApplicationCommand(t, append(arguments, "--api-url", server.URL)...)
		assert.True(err != nil)
		assert.True(errors.Is(err, io.EOF))
		assert.Equal(output, "")
		for _, hint := range []string{server.URL, "connection closed", "borq management bootstrap local", "borq config show", "borq config set --api-url", "--profile NAME"} {
			assert.True(strings.Contains(err.Error(), hint))
		}
	}
	assert.Equal(requests.Load(), int32(3))
}

// TestApplicationTruncatedResponseGuidance covers dropped connections during
// response decoding and prevents partial response values from being printed.
func TestApplicationTruncatedResponseGuidance(t *testing.T) {
	assert := is.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("Content-Length", "1024")
		_, _ = io.WriteString(writer, `{"name":"private-response"}`)
	}))
	defer server.Close()
	output, _, err := runApplicationCommand(t, "org", "create", "testorg", "--api-url", server.URL)
	assert.True(err != nil)
	assert.True(errors.Is(err, io.ErrUnexpectedEOF))
	assert.True(strings.Contains(err.Error(), "connection closed"))
	assert.True(strings.Contains(err.Error(), "borq management bootstrap local"))
	assert.True(!strings.Contains(err.Error(), "private-response"))
	assert.Equal(output, "")
}

// TestApplicationGatewayAndDomainErrors adds setup guidance for unavailable
// gateways while retaining the server's normal validation and conflict messages.
func TestApplicationGatewayAndDomainErrors(t *testing.T) {
	for _, status := range []int{http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout, http.StatusBadRequest, http.StatusNotFound, http.StatusConflict, http.StatusInternalServerError} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			assert := is.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				writer.WriteHeader(status)
				_, _ = io.WriteString(writer, `{"message":"server-provided-details"}`)
			}))
			defer server.Close()
			output, _, err := runApplicationCommand(t, "org", "create", "testorg", "--api-url", server.URL)
			assert.True(err != nil)
			assert.Equal(output, "")
			var connectionError applicationConnectionError
			unavailable := status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout
			assert.Equal(errors.As(err, &connectionError), unavailable)
			assert.True(strings.Contains(err.Error(), fmt.Sprint(status)))
			assert.Equal(strings.Contains(err.Error(), "server-provided-details"), !unavailable)
		})
	}
}

// TestApplicationConnectionReasons separates certificate, DNS, timeout, and
// socket failures and keeps sensitive URLs and transport messages out of output.
func TestApplicationConnectionReasons(t *testing.T) {
	for _, input := range []struct {
		name, server, reason string
		cause                error
		localSetup           bool
	}{
		{"EOF", "http://127.0.0.1:7833", "connection closed", fmt.Errorf("private-cause: %w", io.EOF), true},
		{"truncated", "http://localhost:7833", "connection closed", io.ErrUnexpectedEOF, true},
		{"refused", "http://[::1]:7833", "connection was refused", syscall.ECONNREFUSED, true},
		{"reset", "http://localhost:7833", "connection closed", syscall.ECONNRESET, true},
		{"timeout", "https://remote.test", "request timed out", context.DeadlineExceeded, false},
		{"DNS", "https://remote.test", "hostname could not be resolved", &net.DNSError{Name: "private-cause", IsNotFound: true}, false},
		{"certificate", "https://localhost:7833", "TLS certificate could not be verified", &tls.CertificateVerificationError{Err: errors.New("private-cause")}, false},
		{"remote", "https://localhost.example.test", "connection closed", io.EOF, false},
		{"credentials", "http://private-user:private-password@localhost:7833/api?token=private-token#private-fragment", "connection closed", io.EOF, true},
		{"unknown", "https://remote.test", "connection failed", errors.New("private-cause"), false},
	} {
		t.Run(input.name, func(t *testing.T) {
			assert := is.New(t)
			problem := applicationConnectionError{server: input.server, cause: input.cause}
			message := problem.Error()
			assert.True(strings.Contains(message, input.reason))
			assert.True(errors.Is(problem, input.cause))
			assert.Equal(strings.Contains(message, "borq management bootstrap local"), input.localSetup)
			assert.True(strings.Contains(message, "borq config show"))
			assert.True(!strings.Contains(message, "private-"))
		})
	}
}

// TestApplicationClientContextCompletion keeps user cancellation and local wait
// deadlines distinct from an unavailable management server.
func TestApplicationClientContextCompletion(t *testing.T) {
	assert := is.New(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { requests.Add(1) }))
	defer server.Close()
	client, err := applicationClient(server.URL)
	assert.NoErr(err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	expired, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	for _, completed := range []context.Context{ctx, expired} {
		_, err = client.ListOrganizationsWithResponse(completed)
		assert.True(errors.Is(err, completed.Err()))
		assert.Equal(err.Error(), completed.Err().Error())
	}
	assert.Equal(requests.Load(), int32(0))
}
