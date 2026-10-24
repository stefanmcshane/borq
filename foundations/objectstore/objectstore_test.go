package objectstore

import (
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/matryer/is"
)

// TestS3Protocol verifies signed normal operations, isolated prefixes, cleanup, and safe errors.
func TestS3Protocol(t *testing.T) {
	assert := is.New(t)
	var mutex sync.Mutex
	objects := map[string][]byte{}
	denied := false
	requests := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mutex.Lock()
		defer mutex.Unlock()
		if !strings.HasPrefix(request.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=test-access/") {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		requests = append(requests, request.Method+" "+request.URL.Path)
		if denied {
			writer.WriteHeader(http.StatusForbidden)
			_, _ = writer.Write([]byte(`<Error><Code>AccessDenied</Code><Message>sensitive-service-body</Message></Error>`))
			return
		}
		if request.URL.Query().Get("list-type") == "2" {
			writer.Header().Set("Content-Type", "application/xml")
			type content struct {
				Key  string `xml:"Key"`
				Size int    `xml:"Size"`
			}
			listing := struct {
				XMLName     xml.Name  `xml:"ListBucketResult"`
				Contents    []content `xml:"Contents"`
				IsTruncated bool      `xml:"IsTruncated"`
			}{Contents: []content{}}
			for key, data := range objects {
				if strings.HasPrefix(strings.TrimPrefix(key, "/bucket/"), request.URL.Query().Get("prefix")) {
					listing.Contents = append(listing.Contents, content{Key: strings.TrimPrefix(key, "/bucket/"), Size: len(data)})
				}
			}
			_ = xml.NewEncoder(writer).Encode(listing)
			return
		}
		switch request.Method {
		case http.MethodPut:
			data, err := io.ReadAll(request.Body)
			if err != nil {
				writer.WriteHeader(500)
				return
			}
			objects[request.URL.Path] = data
		case http.MethodGet:
			data, exists := objects[request.URL.Path]
			if !exists {
				writer.WriteHeader(404)
				return
			}
			_, _ = writer.Write(data)
		case http.MethodDelete:
			delete(objects, request.URL.Path)
			writer.WriteHeader(204)
		default:
			writer.WriteHeader(405)
		}
	}))
	defer server.Close()
	client, err := New(Config{Endpoint: server.URL, Region: "us-east-1", Bucket: "bucket", Prefix: "organizations/immutable", PathStyle: true, Credentials: Credentials{AccessKey: "test-access", SecretKey: "test-secret"}})
	assert.NoErr(err)
	assert.NoErr(client.Validate(context.Background()))
	mutex.Lock()
	assert.Equal(len(objects), 0)
	mutex.Unlock()
	assert.NoErr(client.Put(context.Background(), "source/backup.info", []byte("completed")))
	data, err := client.Read(context.Background(), "source/backup.info")
	assert.NoErr(err)
	assert.Equal(string(data), "completed")
	page, err := client.ListPage(context.Background(), "source/", "", 10)
	assert.NoErr(err)
	assert.Equal(len(page.Objects), 1)
	assert.Equal(page.Objects[0].Key, "source/backup.info")
	assert.NoErr(client.Delete(context.Background(), "source/backup.info"))
	for _, key := range []string{"../escape", "/absolute", "nested/../escape"} {
		assert.True(client.Put(context.Background(), key, []byte("bad")) != nil)
	}
	mutex.Lock()
	denied = true
	before := len(requests)
	mutex.Unlock()
	_, err = client.Read(context.Background(), "secret")
	assert.True(err != nil)
	assert.True(!strings.Contains(err.Error(), "sensitive-service-body"))
	assert.True(!strings.Contains(err.Error(), "test-secret"))
	mutex.Lock()
	assert.True(len(requests) > before)
	mutex.Unlock()
}

// TestS3Configuration rejects credential-bearing endpoints and traversal before a request.
func TestS3Configuration(t *testing.T) {
	for _, endpoint := range []string{"ftp://s3.example", "https://user:password@s3.example", "https://s3.example/path", "https://s3.example?key=value"} {
		_, err := New(Config{Endpoint: endpoint, Region: "region", Bucket: "bucket", Credentials: Credentials{AccessKey: "access", SecretKey: "secret"}})
		is.New(t).True(err != nil)
	}
	_, err := New(Config{Endpoint: "https://s3.example", Region: "region", Bucket: "bucket", Prefix: "parent/../outside", Credentials: Credentials{AccessKey: "access", SecretKey: "secret"}})
	is.New(t).True(err != nil)
}
