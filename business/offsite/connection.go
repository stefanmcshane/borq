package offsite

import (
	"context"

	"github.com/stefanmcshane/borq/foundations/objectstore"
)

// TestConnection verifies S3 access within the repository prefix using a disposable object.
func TestConnection(ctx context.Context, repository Repository) error {
	if err := repository.Validate(); err != nil {
		return err
	}
	config, err := repository.storageConfig()
	if err != nil {
		return err
	}
	client, err := objectstore.New(config)
	if err != nil {
		return err
	}
	return client.Validate(ctx)
}
