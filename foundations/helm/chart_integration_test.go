//go:build integration

package helm

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
)

// TestPublicChartDefaultsIntegration verifies that HTTP and OCI transports fetch the
// same pinned upstream values, including comments, without Kubernetes credentials.
func TestPublicChartDefaultsIntegration(t *testing.T) {
	assert := is.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var previous string
	for _, repository := range []string{"https://stefanprodan.github.io/podinfo", "oci://ghcr.io/stefanprodan/charts"} {
		defaults, err := FetchDefaults(ctx, ChartInput{Repository: repository, Name: "podinfo", Version: "6.5.4"})
		assert.NoErr(err)
		assert.True(strings.Contains(defaults.Values, "replicaCount:"))
		assert.True(strings.Contains(defaults.Values, "#"))
		if previous != "" {
			assert.Equal(defaults.Values, previous)
		}
		previous = defaults.Values
	}
}
