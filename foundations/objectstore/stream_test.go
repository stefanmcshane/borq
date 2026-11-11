package objectstore

import (
	"bytes"
	"testing"

	"github.com/matryer/is"
)

// TestStreamSizeBounds keeps export and recovery sizes symmetric and supports ordinary archives larger than 78 GiB.
func TestStreamSizeBounds(t *testing.T) {
	assert := is.New(t)
	for _, size := range []int64{8 << 30, 100 << 30, MaxStreamSize} {
		part := uploadPartSize(size)
		assert.True(part >= 8<<20 && part <= 64<<20)
		assert.True((size+part-1)/part <= 10000)
	}
	client, err := New(Config{Endpoint: "http://127.0.0.1:1", Region: "region", Bucket: "bucket", Credentials: Credentials{AccessKey: "access", SecretKey: "secret"}})
	assert.NoErr(err)
	err = client.PutNew(t.Context(), "object", bytes.NewReader(nil), MaxStreamSize+1)
	assert.True(err != nil && err.Error() == "S3 streamed object size must be between zero and 625 GiB")
}
