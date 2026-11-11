package objectstore

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/matryer/is"
)

// TestS3ServerSideEncryption protects the explicit AES256 contract and rejects providers that do not acknowledge it before sending multipart bodies.
func TestS3ServerSideEncryption(t *testing.T) {
	for _, confirmed := range []bool{true, false} {
		for _, write := range []string{"put", "empty", "multipart"} {
			t.Run(fmt.Sprintf("%s-confirmed-%t", write, confirmed), func(t *testing.T) {
				check := is.New(t)
				bodySent := false
				aborted := false
				server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					writer.Header().Set("Content-Type", "application/xml")
					if request.Method == http.MethodDelete {
						aborted = true
						writer.WriteHeader(http.StatusNoContent)
						return
					}
					if request.URL.Query().Get("partNumber") != "" {
						bodySent = true
						_, _ = io.Copy(io.Discard, request.Body)
						writer.Header().Set("ETag", "\"part\"")
						return
					}
					if request.URL.Query().Get("uploadId") != "" {
						_, _ = fmt.Fprint(writer, `<CompleteMultipartUploadResult><ETag>"finished"</ETag></CompleteMultipartUploadResult>`)
						return
					}
					check.Equal(request.Header.Get("X-Amz-Server-Side-Encryption"), "AES256")
					if confirmed {
						writer.Header().Set("X-Amz-Server-Side-Encryption", "AES256")
					}
					if request.Method == http.MethodPost {
						_, _ = fmt.Fprint(writer, `<InitiateMultipartUploadResult><Bucket>backups</Bucket><Key>archive</Key><UploadId>upload</UploadId></InitiateMultipartUploadResult>`)
					}
				}))
				defer server.Close()
				client, err := New(Config{Endpoint: server.URL, Region: "us-east-1", Bucket: "backups", PathStyle: true, ServerSideEncryption: "AES256", Credentials: Credentials{AccessKey: "access", SecretKey: "secret"}})
				check.NoErr(err)
				switch write {
				case "put":
					err = client.Put(t.Context(), "archive", []byte("backup"))
				case "empty":
					err = client.PutNew(t.Context(), "archive", bytes.NewReader(nil), 0)
				case "multipart":
					err = client.PutNew(t.Context(), "archive", strings.NewReader("backup"), 6)
				}
				if confirmed {
					check.NoErr(err)
				} else {
					check.True(err != nil)
					check.True(strings.Contains(err.Error(), "encryption"))
				}
				if write == "multipart" {
					check.Equal(bodySent, confirmed)
					check.Equal(aborted, !confirmed)
				}
			})
		}
	}
	_, err := New(Config{Region: "us-east-1", Bucket: "backups", ServerSideEncryption: "aws:kms", Credentials: Credentials{AccessKey: "access", SecretKey: "secret"}})
	is.New(t).True(err != nil)
}
