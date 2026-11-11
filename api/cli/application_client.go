package cli

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/stefanmcshane/borq/api/appapi"
)

type applicationHttpClient struct {
	client http.Client
	server string
	token  string
}

type applicationConnectionError struct {
	server     string
	cause      error
	statusCode int
}

type applicationResponseBody struct {
	body   io.ReadCloser
	server string
	ctx    context.Context
}

func applicationClient(server string) (*appapi.ClientWithResponses, error) {
	origin, err := canonicalServerOrigin(server)
	if err != nil {
		return nil, err
	}
	path, err := credentialPath(origin)
	if err != nil {
		return nil, err
	}
	token, err := readPrivateToken(path)
	if errors.Is(err, os.ErrNotExist) {
		token, err = "", nil
	}
	if err != nil {
		return nil, err
	}
	return applicationClientWithToken(origin, token)
}

// A command-scoped token bypasses saved profiles without persisting credentials.
func applicationClientForCommand(server, tokenFile string) (*appapi.ClientWithResponses, error) {
	if tokenFile == "" {
		return applicationClient(server)
	}
	token, err := readPrivateToken(tokenFile)
	if err != nil {
		return nil, err
	}
	return applicationClientWithToken(server, token)
}

func applicationClientWithToken(server, token string) (*appapi.ClientWithResponses, error) {
	origin, err := canonicalServerOrigin(server)
	if err != nil {
		return nil, err
	}
	if token != "" {
		if err := validateCredentialTransport(origin); err != nil {
			return nil, err
		}
	}
	client := applicationHttpClient{client: http.Client{Timeout: 15 * time.Minute, CheckRedirect: refuseAuthenticationRedirect}, server: origin, token: token}
	return appapi.NewClientWithResponses(strings.TrimRight(server, "/"), appapi.WithHTTPClient(client))
}

// Do adds recovery guidance to transport and gateway failures for every API
// command. Request and response pointers are required by net/http.
func (client applicationHttpClient) Do(request *http.Request) (*http.Response, error) {
	if client.token != "" {
		origin, err := canonicalServerOrigin(request.URL.Scheme + "://" + request.URL.Host)
		if err != nil || origin != client.server || request.URL.User != nil {
			return nil, errors.New("refusing to send administrator token to another server")
		}
		request = request.Clone(request.Context())
		request.Header.Set("Authorization", "Bearer "+client.token)
	}
	response, err := client.client.Do(request)
	if err != nil {
		if contextError := request.Context().Err(); contextError != nil {
			return nil, contextError
		}
		return nil, applicationConnectionError{server: client.server, cause: err}
	}
	if response.StatusCode == http.StatusUnauthorized {
		_ = response.Body.Close()
		return nil, fmt.Errorf("Borq requires login; run borq login --api-url %s --token-file PATH using a private API token file; for initial setup, export management keys with borq management keys and log in with --token-file pointing to that file", client.server)
	}
	// Only Borq's explicit pre-submission setup rejection is safe for apply to retry.
	setupPending := request.Method == http.MethodPut && strings.HasSuffix(request.URL.Path, "/api/v1/management/configuration") && response.StatusCode == http.StatusServiceUnavailable && response.Header.Get("X-Borq-Setup-Pending") == "true"
	if !setupPending && (response.StatusCode == http.StatusBadGateway || response.StatusCode == http.StatusServiceUnavailable || response.StatusCode == http.StatusGatewayTimeout) {
		_ = response.Body.Close()
		return nil, applicationConnectionError{server: client.server, statusCode: response.StatusCode}
	}
	response.Body = applicationResponseBody{body: response.Body, server: client.server, ctx: request.Context()}
	return response, nil
}

// Read also covers connections dropped after HTTP headers have arrived, while
// preserving ordinary EOF and the caller's cancellation or wait deadline.
func (response applicationResponseBody) Read(buffer []byte) (int, error) {
	count, err := response.body.Read(buffer)
	if err == nil || errors.Is(err, io.EOF) {
		return count, err
	}
	if contextError := response.ctx.Err(); contextError != nil {
		return count, contextError
	}
	return count, applicationConnectionError{server: response.server, cause: err}
}

// Close releases the underlying connection normally after generated decoding.
func (response applicationResponseBody) Close() error { return response.body.Close() }

// Error describes what failed and how to recover without echoing request paths,
// credentials, response bodies, or an unverified claim about cluster existence.
func (problem applicationConnectionError) Error() string {
	endpoint, err := url.Parse(problem.server)
	if err != nil {
		return "cannot contact the Borq management API; check the URL with borq config show"
	}
	endpoint.User, endpoint.RawQuery, endpoint.Fragment = nil, "", ""
	endpoint.ForceQuery = false
	var certificateError *tls.CertificateVerificationError
	var dnsError *net.DNSError
	var networkError net.Error
	reason := "connection failed"
	configurationIssue := false
	switch {
	case problem.statusCode != 0:
		reason = fmt.Sprintf("server returned HTTP %d (%s)", problem.statusCode, http.StatusText(problem.statusCode))
	case errors.As(problem.cause, &certificateError):
		reason = "the TLS certificate could not be verified; check the API hostname and certificate trust"
		configurationIssue = true
	case errors.As(problem.cause, &dnsError):
		reason = "the API hostname could not be resolved; check the hostname and DNS"
		configurationIssue = true
	case errors.As(problem.cause, &networkError) && networkError.Timeout():
		reason = "the request timed out"
	case errors.Is(problem.cause, io.EOF), errors.Is(problem.cause, io.ErrUnexpectedEOF), errors.Is(problem.cause, syscall.ECONNRESET), errors.Is(problem.cause, syscall.EPIPE):
		reason = "the connection closed before a complete response was received"
	case errors.Is(problem.cause, syscall.ECONNREFUSED):
		reason = "the connection was refused"
	}
	message := fmt.Sprintf("Borq management API at %s is unavailable: %s.\n\n", endpoint.String(), reason)
	address := net.ParseIP(endpoint.Hostname())
	local := strings.EqualFold(endpoint.Hostname(), "localhost") || (address != nil && address.IsLoopback())
	if local && !configurationIssue {
		message += "Check that the existing k3s cluster is running, then bootstrap Borq using its kubeconfig if needed:\n" +
			"  borq management bootstrap local --kubeconfig PATH\n" +
			"For an existing setup, replace local with your management cluster's name.\n\n"
	} else if !configurationIssue {
		message += "Check that the management server is running and reachable from this machine.\n\n"
	}
	return message + "Check or change the selected API:\n  borq config show\n  borq config set --api-url URL\n" +
		"Use the same --profile NAME on these commands if you selected a profile."
}

// Unwrap preserves cancellation, timeout, and transport causes for callers.
func (problem applicationConnectionError) Unwrap() error { return problem.cause }
