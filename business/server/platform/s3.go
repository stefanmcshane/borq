package platform

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"

	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
)

// S3Settings configures shared management storage without raw credentials.
type S3Settings struct {
	// Endpoint is the shared S3 HTTP(S) origin.
	Endpoint string `json:"endpoint,omitempty"`
	// Region selects the request signing region.
	Region string `json:"region,omitempty"`
	// Bucket contains Loki and isolated managed-addon archives.
	Bucket string `json:"bucket,omitempty"`
	// Prefix isolates managed-addon archives; existing Loki object keys remain unchanged.
	Prefix string `json:"prefix,omitempty"`
	// PathStyle supports S3-compatible services.
	PathStyle bool `json:"pathStyle"`
	// CredentialsRef selects a management-vault document.
	CredentialsRef string `json:"credentialsRef,omitempty"`
}

// ManagementBucketName gives each installation a stable S3-compatible data bucket.
func ManagementBucketName(clusterName string) string {
	name := strings.Map(func(character rune) rune {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '-' {
			return character
		}
		return '-'
	}, strings.ToLower(strings.TrimSpace(clusterName)))
	name = strings.Trim(name, "-")
	if name == "" {
		name = "management"
	}
	bucket := "borq-" + name + "-data"
	if len(bucket) > 63 {
		digest := sha256.Sum256([]byte(bucket))
		bucket = fmt.Sprintf("%s-%x", strings.TrimRight(bucket[:50], "-"), digest[:6])
	}
	return bucket
}

func sharedS3SecretName(credentialsRef string) string {
	// Each credential generation must wait for its own ESO projection before Loki starts.
	digest := sha256.Sum256([]byte(credentialsRef))
	return fmt.Sprintf("borq-management-s3-%x", digest[:16])
}

func prepareSharedS3(ctx context.Context, connection kubernetes.Connection, settings Settings) error {
	if settings.S3.CredentialsRef == "" {
		return nil
	}
	if settings.S3.Bucket == "" {
		return errors.New("shared S3 bucket is required")
	}
	store := secretStore("borq-management-openbao", settings.Namespace, "http://openbao."+settings.Namespace+".svc:8200", "secret", "borq-openbao-admin")
	if err := connection.Objects().Apply(ctx, store); err != nil {
		return err
	}
	name := sharedS3SecretName(settings.S3.CredentialsRef)
	if err := connection.Objects().Apply(ctx, externalSecret(name, settings.Namespace, "borq-management-openbao", settings.S3.CredentialsRef)); err != nil {
		return err
	}
	return waitForSecret(ctx, connection, settings.Namespace, name, []string{"accessKey", "secretKey"}, settings.Timeout)
}

// ReconcileManagementStorage projects shared S3 credentials and rolls the existing Loki chart.
func ReconcileManagementStorage(ctx context.Context, input ManagementInput) (ComponentState, error) {
	settings, err := normalizeSettings(input.Settings)
	if err != nil {
		return ComponentState{}, err
	}
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: input.KubeconfigPath}, logger.Logger{})
	if err != nil {
		return ComponentState{}, err
	}
	for _, definition := range managementCatalog(settings, false) {
		if definition.Name != "loki" {
			continue
		}
		run := componentRun{connection: connection, kubeconfigPath: input.KubeconfigPath, values: valuesInput{Settings: settings}, upgrade: true, recoverPending: true}
		return run.reconcile(ctx, definition)
	}
	return ComponentState{}, errors.New("management Loki component is unavailable")
}
