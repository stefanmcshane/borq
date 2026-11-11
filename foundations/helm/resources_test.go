package helm

import (
	"testing"

	"github.com/matryer/is"
)

// TestManifestIdentities reads names from multi-document charts and nested Lists,
// retaining explicit namespaces without returning any Secret configuration.
func TestManifestIdentities(t *testing.T) {
	assert := is.New(t)
	resources, err := manifestIdentities(`
apiVersion: v1
kind: List
items:
  - apiVersion: apps/v1
    kind: StatefulSet
    metadata:
      name: custom-nats
  - apiVersion: v1
    kind: Secret
    metadata:
      name: credentials
      namespace: separate
    stringData:
      password: private
---
# empty document
---
apiVersion: batch/v1
kind: Job
metadata:
  name: migration
`, "addon-space")
	assert.NoErr(err)
	assert.Equal(resources, []ResourceIdentity{
		{Kind: "StatefulSet", Namespace: "addon-space", Name: "custom-nats"},
		{Kind: "Secret", Namespace: "separate", Name: "credentials"},
		{Kind: "Job", Namespace: "addon-space", Name: "migration"},
	})
	_, err = manifestIdentities("kind: [invalid yaml", "addon-space")
	assert.True(err != nil)
}
