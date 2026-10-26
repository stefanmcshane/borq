package postgres

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/objectstore"
)

// TestCatalogueMetadataBounds rejects unbounded live inventories before any body GET and validates actual downloaded sizes.
func TestCatalogueMetadataBounds(t *testing.T) {
	for _, test := range []struct {
		name  string
		count int
		size  int64
		guard string
		gets  int32
	}{
		{"count", 10001, 10, "catalogue exceeds supported bound", 0},
		{"individual", 1, (1 << 20) + 1, "backup.info exceeds supported 1 MiB", 0},
		{"aggregate", 33, 1 << 20, "metadata exceeds supported 32 MiB", 0},
		{"body-size", 1, 123, "body does not match captured size", 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			var gets atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Query().Get("list-type") == "2" {
					type item struct {
						Key  string
						Size int64
					}
					listing := struct {
						XMLName               xml.Name `xml:"ListBucketResult"`
						Contents              []item
						IsTruncated           bool
						NextContinuationToken string
					}{}
					start, _ := strconv.Atoi(request.URL.Query().Get("continuation-token"))
					end := min(start+1000, test.count)
					listing.IsTruncated = end < test.count
					if listing.IsTruncated {
						listing.NextContinuationToken = strconv.Itoa(end)
					}
					for index := start; index < end; index++ {
						listing.Contents = append(listing.Contents, item{Key: fmt.Sprintf("server/base/backup-%d/backup.info", index), Size: test.size})
					}
					_ = xml.NewEncoder(writer).Encode(listing)
					return
				}
				gets.Add(1)
				_, _ = writer.Write([]byte("status=STARTED\n"))
			}))
			defer server.Close()
			_, err := Catalogue(t.Context(), objectstore.Config{Endpoint: server.URL, Region: "region", Bucket: "bucket", PathStyle: true, Credentials: objectstore.Credentials{AccessKey: "access", SecretKey: "secret"}}, "server")
			assert.True(err != nil && strings.Contains(err.Error(), test.guard))
			assert.Equal(gets.Load(), test.gets)
		})
	}
}
