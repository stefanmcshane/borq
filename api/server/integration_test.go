//go:build integration
// +build integration

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

const (
	integrationServerURLEnvVar  = "BORQ_INTEGRATION_SERVER_URL"
	defaultIntegrationServerURL = "http://localhost:7833"
)

func TestServerIntegration(t *testing.T) {
	serverURL := strings.TrimSpace(os.Getenv(integrationServerURLEnvVar))
	if serverURL == "" {
		serverURL = defaultIntegrationServerURL
	}
	serverURL = strings.TrimRight(serverURL, "/")

	testContext, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := assertHealthResponse(testContext, serverURL+"/healthz"); err != nil {
		t.Fatalf("assert /healthz endpoint: %v", err)
	}
	if err := assertHTTPResponse(testContext, http.MethodGet, serverURL+"/", http.StatusOK, "borq"); err != nil {
		t.Fatalf("assert / endpoint: %v", err)
	}
	if err := assertHTTPResponse(testContext, http.MethodPost, serverURL+"/healthz", http.StatusMethodNotAllowed, "method not allowed"); err != nil {
		t.Fatalf("assert /healthz method handling: %v", err)
	}
}

func assertHealthResponse(ctx context.Context, url string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}

	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("perform request: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status: got %d want %d", response.StatusCode, http.StatusOK)
	}

	var report HealthReport
	if err := json.NewDecoder(response.Body).Decode(&report); err != nil {
		return fmt.Errorf("decode health response: %w", err)
	}
	if report.Status != healthStatusSuccess {
		return fmt.Errorf("unexpected health status: got %q want %q", report.Status, healthStatusSuccess)
	}
	if len(report.Checks) == 0 {
		return fmt.Errorf("health response contained no checks")
	}
	for _, check := range report.Checks {
		if strings.TrimSpace(check.Name) == "" {
			return fmt.Errorf("health check is missing name")
		}
		if check.Status != healthStatusSuccess && check.Status != healthStatusFail {
			return fmt.Errorf("health check %q has invalid status %q", check.Name, check.Status)
		}
		if strings.TrimSpace(check.Description) == "" {
			return fmt.Errorf("health check %q is missing description", check.Name)
		}
	}

	return nil
}

func assertHTTPResponse(ctx context.Context, method string, url string, expectedStatus int, expectedBodySubstring string) error {
	request, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}

	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("perform request: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode != expectedStatus {
		return fmt.Errorf("unexpected status: got %d want %d", response.StatusCode, expectedStatus)
	}

	body, err := io.ReadAll(response.Body)
	if err != nil {
		return fmt.Errorf("read response body: %w", err)
	}
	if !strings.Contains(string(body), expectedBodySubstring) {
		return fmt.Errorf("response body %q does not contain %q", string(body), expectedBodySubstring)
	}

	return nil
}
