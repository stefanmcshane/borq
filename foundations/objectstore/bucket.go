package objectstore

import (
	"context"
	"errors"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// EnsureBucket creates a missing bucket without interpreting denied access as absence.
func (client Client) EnsureBucket(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	head := &s3.HeadBucketInput{Bucket: aws.String(client.config.Bucket)}
	if _, err := client.client.HeadBucket(ctx, head); err != nil {
		var response interface{ HTTPStatusCode() int }
		if !errors.As(err, &response) || response.HTTPStatusCode() != 404 {
			return requestError(ctx, "inspect bucket")
		}
	} else {
		return nil
	}
	input := &s3.CreateBucketInput{Bucket: aws.String(client.config.Bucket)}
	if client.config.Region != "us-east-1" {
		input.CreateBucketConfiguration = &types.CreateBucketConfiguration{LocationConstraint: types.BucketLocationConstraint(client.config.Region)}
	}
	if _, err := client.client.CreateBucket(ctx, input); err != nil {
		if _, headError := client.client.HeadBucket(ctx, head); headError != nil {
			return requestError(ctx, "create bucket")
		}
	}
	return nil
}
