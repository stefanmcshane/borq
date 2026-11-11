package sqliteworkload

import (
	"encoding/json"
	"testing"

	"github.com/matryer/is"
	corev1 "k8s.io/api/core/v1"
)

// TestConfig owns the common backend policy and wire field names; callers own
// their database paths, storage coordinates and immutable migration checks.
func TestConfig(t *testing.T) {
	for _, interval := range []string{"", "1s"} {
		t.Run("sync="+interval, func(t *testing.T) {
			is := is.New(t)
			configuration := Config("/data/app.db", Destination{Endpoint: "http://storage:9000", Region: "us-east-1", Bucket: "application", Prefix: "sqlite", PathStyle: true, SyncInterval: interval})
			encoded, err := json.Marshal(configuration)
			is.NoErr(err)
			var wire struct {
				Databases []struct {
					Replicas []map[string]any `json:"replicas"`
				} `json:"dbs"`
			}
			is.NoErr(json.Unmarshal(encoded, &wire))
			replica := wire.Databases[0].Replicas[0]
			is.Equal(replica["type"], "s3")
			is.Equal(replica["age"], map[string]any{"identities": []any{"${LITESTREAM_AGE_IDENTITY}"}, "recipients": []any{"${LITESTREAM_AGE_RECIPIENT}"}})
			is.Equal(replica["retention"], "168h")
			is.Equal(replica["snapshot-interval"], "24h")
			is.Equal(replica["force-path-style"], true)
			if interval == "" {
				_, exists := replica["sync-interval"]
				is.True(!exists)
			} else {
				is.Equal(replica["sync-interval"], interval)
			}
		})
	}
}

// TestReplicationContainer owns native-sidecar ordering and the shared backend
// invocation; application rendering and live Pocket ID tests verify integration.
func TestReplicationContainer(t *testing.T) {
	is := is.New(t)
	container := ReplicationContainer(ReplicationInput{Name: "replication", Directory: "/data", DataVolume: "database", ConfigVolume: "configuration", CredentialSecret: "archive", PullPolicy: corev1.PullAlways})
	is.Equal(*container.RestartPolicy, corev1.ContainerRestartPolicyAlways)
	is.Equal(container.Command, []string{"litestream"})
	is.Equal(container.Args, []string{"replicate", "-config", "/etc/litestream/litestream.yml"})
	is.Equal(container.EnvFrom[0].SecretRef.Name, "archive")
	is.Equal(container.VolumeMounts, []corev1.VolumeMount{{Name: "database", MountPath: "/data"}, {Name: "configuration", MountPath: "/etc/litestream", ReadOnly: true}})
	is.Equal(container.ImagePullPolicy, corev1.PullAlways)
}
