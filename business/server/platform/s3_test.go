package platform

import (
	"regexp"
	"strings"
	"testing"

	"github.com/matryer/is"
)

// TestManagementBucketName keeps installation names recognizable while producing
// valid bucket names for short, unusual, and long bootstrap names.
func TestManagementBucketName(t *testing.T) {
	assert := is.New(t)
	for _, input := range []struct{ name, bucket string }{
		{"", "borq-management-data"},
		{"harbor", "borq-harbor-data"},
		{"a", "borq-a-data"},
		{" North_America.East ", "borq-north-america-east-data"},
		{"xn--reserved--x-s3", "borq-xn--reserved--x-s3-data"},
	} {
		assert.Equal(ManagementBucketName(input.name), input.bucket)
	}
	first := ManagementBucketName(strings.Repeat("a", 80))
	second := ManagementBucketName(strings.Repeat("a", 79) + "b")
	assert.True(first != second)
	assert.True(len(first) <= 63)
	assert.True(regexp.MustCompile(`^borq-[a-z0-9-]+[a-z0-9]$`).MatchString(first))
}
