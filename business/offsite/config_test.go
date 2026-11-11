package offsite

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/objectstore"
)

// TestPrivateConfiguration owns private-file permissions and strict typed recovery inputs.
func TestPrivateConfiguration(t *testing.T) {
	assert := is.New(t)
	for _, test := range []struct {
		name, body string
		mode       os.FileMode
		valid      bool
	}{
		{"private", `{"source":"source","offsite":{"repository":"s3:https://s3.example/backups/borq","password":"retained-password","accessKey":"access","secretKey":"secret","region":"region"}}`, 0600, true},
		{"public", `{}`, 0644, false},
		{"unknown", `{"secretSpelling":"private"}`, 0600, false},
		{"trailing", `{} {}`, 0600, false},
		{"oversize", strings.Repeat(" ", 2<<20) + `{}`, 0600, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "recovery.json")
			assert.NoErr(os.WriteFile(file, []byte(test.body), test.mode))
			var config RecoveryConfig
			err := ReadPrivateConfig(file, &config)
			assert.Equal(err == nil, test.valid)
			if test.valid {
				assert.NoErr(config.Repository.Validate())
				assert.Equal(config.Repository.Password, "retained-password")
			}
		})
	}
	for _, url := range []string{"s3:https://user:private@s3.example/backups", "s3:https://s3.example/backups?password=private", "s3:https://s3.example/backups#private", "sftp:private@example:/backups"} {
		err := (Repository{URL: url, Password: "private", AccessKey: "private", SecretKey: "private", Region: "region"}).Validate()
		assert.True(err != nil)
		assert.True(!strings.Contains(err.Error(), "private"))
	}
}

// TestEmptyCaptureIsNotProtected rejects a misleading successful snapshot before any complete PostgreSQL backup exists.
func TestEmptyCaptureIsNotProtected(t *testing.T) {
	assert := is.New(t)
	fixture := newArchiveS3(t)
	var output strings.Builder
	err := Export(t.Context(), fixture.config("empty"), &output)
	assert.True(err != nil && strings.Contains(err.Error(), "no complete managed backups"))
	assert.Equal(output.Len(), 0)
}

// TestRepositorySeparation rejects same-bucket recursive capture even when endpoint aliases differ.
func TestRepositorySeparation(t *testing.T) {
	assert := is.New(t)
	repository := Repository{URL: "s3:https://s3.example/bucket/organizations/archive/base/offsite", Password: "password", AccessKey: "access", SecretKey: "secret", Region: "region"}
	err := ValidateSeparation(repository, objectstore.Config{Endpoint: "https://alias.example", Bucket: "bucket"})
	assert.True(err != nil && strings.Contains(err.Error(), "separate external S3 bucket"))
	repository.URL = "s3:https://s3.example/independent/borq"
	assert.NoErr(ValidateSeparation(repository, objectstore.Config{Bucket: "bucket"}))
}
