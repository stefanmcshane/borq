package openbao_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"

	"github.com/stefanmcshane/borq/foundations/openbao"
)

// TestKvReadsPinnedAndLatestVersions verifies explicit version queries, the
// typed storage envelope, and service-proxy URLs with a custom TLS transport.
func TestKvReadsPinnedAndLatestVersions(t *testing.T) {
	assert := is.New(t)
	const prefix = "/api/v1/namespaces/borq-system/services/http:openbao:8200/proxy"
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		assert.Equal(request.Method, http.MethodGet)
		assert.Equal(request.URL.Path, prefix+"/v1/secret/data/org/project/app")
		assert.Equal(request.Header.Get("X-Vault-Token"), "explicit-test-token")
		version := 7
		if requests == 0 {
			assert.Equal(request.URL.Query().Get("version"), "3")
			version = 3
		} else {
			assert.Equal(request.URL.Query().Get("version"), "")
		}
		requests++
		fmt.Fprintf(w, `{"data":{"data":{"values":{"EMPTY":"","TOKEN":"sensitive-value"},"planId":"plan-1"},"metadata":{"version":%d,"deletion_time":"","destroyed":false}}}`, version)
	}))
	defer server.Close()
	client, err := openbao.NewClient(openbao.NewClientInput{
		Address: server.URL + prefix, Token: "explicit-test-token", HttpClient: *server.Client(),
	})
	assert.NoErr(err)
	for _, version := range []int{3, 0} {
		entry, err := client.ReadKv(t.Context(), "secret", "org/project/app", version)
		assert.NoErr(err)
		assert.Equal(entry.Values, map[string]string{"EMPTY": "", "TOKEN": "sensitive-value"})
		assert.Equal(entry.PlanID, "plan-1")
		if version == 3 {
			assert.Equal(entry.Version, 3)
		} else {
			assert.Equal(entry.Version, 7)
		}
	}
	assert.Equal(requests, 2)
}

// TestKvWriteAlwaysChecksVersion ensures both creation and replacement use CAS,
// preserve empty string values, and return a detached copy of supplied data.
func TestKvWriteAlwaysChecksVersion(t *testing.T) {
	assert := is.New(t)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		assert.Equal(request.Method, http.MethodPut)
		assert.Equal(request.URL.Path, "/v1/secret/data/project/env/app")
		var body struct {
			Options map[string]int `json:"options"`
			Data    struct {
				Values map[string]string `json:"values"`
				PlanID string            `json:"planId"`
			} `json:"data"`
		}
		assert.NoErr(json.NewDecoder(request.Body).Decode(&body))
		expected := []int{0, 3}[requests]
		assert.Equal(body.Options, map[string]int{"cas": expected})
		assert.Equal(body.Data.Values, map[string]string{"TOKEN": "sensitive-value", "EMPTY": ""})
		assert.Equal(body.Data.PlanID, "plan-1")
		requests++
		fmt.Fprintf(w, `{"data":{"version":%d,"deletion_time":"","destroyed":false}}`, expected+1)
	}))
	defer server.Close()
	client, err := openbao.NewClient(openbao.NewClientInput{Address: server.URL})
	assert.NoErr(err)
	for _, version := range []int{0, 3} {
		values := map[string]string{"TOKEN": "sensitive-value", "EMPTY": ""}
		entry, err := client.WriteKv(t.Context(), "secret", "project/env/app", openbao.WriteKvInput{
			Values: values, PlanID: "plan-1", CheckVersion: version,
		})
		assert.NoErr(err)
		assert.Equal(entry.Version, version+1)
		assert.Equal(entry.PlanID, "plan-1")
		values["TOKEN"] = "changed"
		assert.Equal(entry.Values["TOKEN"], "sensitive-value")
	}
	assert.Equal(requests, 2)
}

// TestKvRetentionIsPerKey verifies that changing retention touches only the
// selected key's metadata and does not replace unrelated metadata settings.
func TestKvRetentionIsPerKey(t *testing.T) {
	assert := is.New(t)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		assert.Equal(request.URL.Path, "/v1/secret/metadata/project/env/app")
		if request.Method == http.MethodPut {
			var body map[string]int
			assert.NoErr(json.NewDecoder(request.Body).Decode(&body))
			assert.Equal(body, map[string]int{"max_versions": 10})
			w.WriteHeader(http.StatusNoContent)
		} else {
			assert.Equal(request.Method, http.MethodGet)
			fmt.Fprint(w, `{"data":{"current_version":12,"oldest_version":3,"max_versions":10,"versions":{"12":{"version":12,"destroyed":false,"deletion_time":""}}}}`)
		}
		requests++
	}))
	defer server.Close()
	client, err := openbao.NewClient(openbao.NewClientInput{Address: server.URL})
	assert.NoErr(err)
	assert.NoErr(client.SetKvRetention(t.Context(), "secret", "project/env/app", 10))
	metadata, err := client.ReadKvMetadata(t.Context(), "secret", "project/env/app")
	assert.NoErr(err)
	assert.Equal(metadata, openbao.KvMetadata{CurrentVersion: 12, OldestVersion: 3, MaxVersions: 10})
	assert.Equal(requests, 2)
}

// TestKvErrorsHideBackendData keeps secret values, credentials, and response
// bodies out of errors while exposing missing entries and CAS races safely.
func TestKvErrorsHideBackendData(t *testing.T) {
	for _, test := range []struct {
		name     string
		status   int
		body     string
		expected error
	}{
		{name: "missing", status: http.StatusNotFound, body: `{"errors":[]}`, expected: openbao.ErrNotFound},
		{name: "conflict", status: http.StatusBadRequest, body: `{"errors":["check-and-set parameter did not match the current version: secret-value"]}`, expected: openbao.ErrConflict},
		{name: "invalid request", status: http.StatusBadRequest, body: `{"errors":["invalid secret-value"]}`},
		{name: "forbidden", status: http.StatusForbidden, body: `{"errors":["secret-token forbidden"]}`},
		{name: "proxy error", status: http.StatusBadGateway, body: `<html>secret-value secret-token</html>`},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				w.WriteHeader(test.status)
				fmt.Fprint(w, test.body)
			}))
			defer server.Close()
			client, err := openbao.NewClient(openbao.NewClientInput{Address: server.URL, Token: "secret-token"})
			assert.NoErr(err)
			for _, request := range []func() error{
				func() error { _, err := client.ReadKv(t.Context(), "secret", "app", 0); return err },
				func() error { _, err := client.ReadKvMetadata(t.Context(), "secret", "app"); return err },
				func() error { return client.SetKvRetention(t.Context(), "secret", "app", 10) },
				func() error {
					_, err := client.WriteKv(t.Context(), "secret", "app", openbao.WriteKvInput{})
					return err
				},
			} {
				err := request()
				assert.True(err != nil)
				if test.expected != nil {
					assert.True(errors.Is(err, test.expected))
				}
				assert.True(!strings.Contains(err.Error(), "secret-value"))
				assert.True(!strings.Contains(err.Error(), "secret-token"))
				assert.True(!strings.Contains(err.Error(), server.URL))
			}
		})
	}
}

// TestKvReadRejectsMalformedDocuments verifies that only the agreed string
// envelope can reach callers and soft-deleted versions report not found.
func TestKvReadRejectsMalformedDocuments(t *testing.T) {
	for _, test := range []struct {
		name     string
		data     string
		version  int
		notFound bool
	}{
		{name: "deleted", data: `null`, version: 1, notFound: true},
		{name: "missing envelope", data: `{"TOKEN":"secret-value"}`, version: 1},
		{name: "non-string value", data: `{"values":{"TOKEN":{"secret-value":true}}}`, version: 1},
		{name: "invalid tag", data: `{"values":{},"planId":{"secret-value":true}}`, version: 1},
		{name: "invalid version", data: `{"values":{"TOKEN":"secret-value"}}`, version: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				fmt.Fprintf(w, `{"data":{"data":%s,"metadata":{"version":%d,"deletion_time":"","destroyed":false}}}`, test.data, test.version)
			}))
			defer server.Close()
			client, err := openbao.NewClient(openbao.NewClientInput{Address: server.URL})
			assert.NoErr(err)
			_, err = client.ReadKv(t.Context(), "secret", "app", 0)
			assert.True(err != nil)
			assert.Equal(errors.Is(err, openbao.ErrNotFound), test.notFound)
			assert.True(!strings.Contains(err.Error(), "secret-value"))
		})
	}
}

// TestKvRejectsInvalidRequests prevents a malformed key from changing the SDK
// route and verifies cancellation remains identifiable without leaking data.
func TestKvRejectsInvalidRequests(t *testing.T) {
	assert := is.New(t)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	client, err := openbao.NewClient(openbao.NewClientInput{Address: server.URL})
	assert.NoErr(err)
	for _, path := range []string{"", "..", "app/../other", "app//other", "app/./other"} {
		_, err := client.ReadKv(t.Context(), "secret", path, 0)
		assert.True(err != nil)
		_, err = client.ReadKvMetadata(t.Context(), "secret", path)
		assert.True(err != nil)
		assert.True(client.SetKvRetention(t.Context(), "secret", path, 10) != nil)
		_, err = client.WriteKv(t.Context(), "secret", path, openbao.WriteKvInput{})
		assert.True(err != nil)
	}
	_, err = client.ReadKv(t.Context(), "secret", "app", -1)
	assert.True(err != nil)
	_, err = client.ReadKv(t.Context(), "../sys", "app", 0)
	assert.True(err != nil)
	assert.True(client.SetKvRetention(t.Context(), "secret", "app", -1) != nil)
	_, err = client.WriteKv(t.Context(), "secret", "app", openbao.WriteKvInput{CheckVersion: -1})
	assert.True(err != nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = client.ReadKv(ctx, "secret", "app", 0)
	assert.True(errors.Is(err, context.Canceled))
	assert.Equal(requests, 0)
}

// TestClientUsesExplicitCredentials ensures the SDK cannot silently use an
// ambient token or namespace when a caller supplies its connection settings.
func TestClientUsesExplicitCredentials(t *testing.T) {
	assert := is.New(t)
	t.Setenv("BAO_TOKEN", "ambient-token")
	t.Setenv("BAO_NAMESPACE", "ambient-namespace")
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		assert.Equal(request.Header.Get("X-Vault-Token"), "")
		assert.Equal(request.Header.Get("X-Vault-Namespace"), "")
		requests++
		fmt.Fprint(w, `{"data":{"version":1,"deletion_time":"","destroyed":false}}`)
	}))
	defer server.Close()
	client, err := openbao.NewClient(openbao.NewClientInput{Address: server.URL, RequestTimeout: time.Second})
	assert.NoErr(err)
	entry, err := client.WriteKv(t.Context(), "secret", "app", openbao.WriteKvInput{})
	assert.NoErr(err)
	assert.Equal(entry.Values, map[string]string{})
	assert.Equal(requests, 1)
}
