package objectstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// MaxStreamSize bounds one streamed object to 625 GiB with at most 10000 parts of 64 MiB.
const MaxStreamSize int64 = 10000 * (64 << 20)

// OpenCaptured streams exactly one captured object, failing if its ETag or size changed.
// The caller owns the returned body and must close it before canceling ctx.
func (client Client) OpenCaptured(ctx context.Context, object Object) (io.ReadCloser, error) {
	if object.ETag == "" || object.Size < 0 {
		return nil, errors.New("S3 capture requires an ETag and nonnegative size")
	}
	key, err := scopedKey(client.config.Prefix, object.Key)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Hour)
	response, err := client.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(client.config.Bucket), Key: aws.String(key), IfMatch: aws.String(object.ETag)})
	if err != nil {
		failure := requestError(ctx, "conditional read")
		cancel()
		return nil, failure
	}
	if aws.ToString(response.ETag) != object.ETag || aws.ToInt64(response.ContentLength) != object.Size {
		response.Body.Close()
		cancel()
		return nil, errors.New("S3 captured identity changed")
	}
	return capturedBody{ReadCloser: response.Body, cancel: cancel}, nil
}

// PutNew streams an object in size-aware 8 to 64 MiB parts and atomically rejects existing keys.
// Multipart uploads are aborted on failure, including source truncation and cancellation.
func (client Client) PutNew(ctx context.Context, key string, body io.Reader, size int64) error {
	if size < 0 || size > MaxStreamSize {
		return errors.New("S3 streamed object size must be between zero and 625 GiB")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Hour)
	defer cancel()
	key, err := scopedKey(client.config.Prefix, key)
	if err != nil {
		return err
	}
	if size == 0 {
		_, err = client.client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(client.config.Bucket), Key: aws.String(key), Body: bytes.NewReader(nil), IfNoneMatch: aws.String("*")})
		if err != nil {
			return requestError(ctx, "create")
		}
		return nil
	}
	upload, err := client.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(client.config.Bucket), Key: aws.String(key)})
	if err != nil {
		return requestError(ctx, "create upload")
	}
	complete := false
	defer func() {
		if complete {
			return
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		_, _ = client.client.AbortMultipartUpload(cleanup, &s3.AbortMultipartUploadInput{Bucket: aws.String(client.config.Bucket), Key: aws.String(key), UploadId: upload.UploadId})
	}()
	parts := []types.CompletedPart{}
	buffer := make([]byte, uploadPartSize(size))
	remaining := size
	for remaining > 0 {
		if len(parts) == 10000 {
			return errors.New("S3 object exceeds supported multipart upload size")
		}
		count := min(int64(len(buffer)), remaining)
		if _, err := io.ReadFull(body, buffer[:count]); err != nil {
			return errors.New("S3 upload source is truncated")
		}
		number := int32(len(parts) + 1)
		part, err := client.client.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String(client.config.Bucket), Key: aws.String(key), UploadId: upload.UploadId, PartNumber: aws.Int32(number), Body: bytes.NewReader(buffer[:count]), ContentLength: aws.Int64(count)})
		if err != nil {
			return requestError(ctx, "upload part")
		}
		parts = append(parts, types.CompletedPart{ETag: part.ETag, PartNumber: aws.Int32(number)})
		remaining -= count
	}
	_, err = client.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String(client.config.Bucket), Key: aws.String(key), UploadId: upload.UploadId, IfNoneMatch: aws.String("*"), MultipartUpload: &types.CompletedMultipartUpload{Parts: parts}})
	if err != nil {
		return requestError(ctx, "complete new object")
	}
	complete = true
	return nil
}

type capturedBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (body capturedBody) Close() error { defer body.cancel(); return body.ReadCloser.Close() }
func uploadPartSize(size int64) int64  { return max(int64(8<<20), (size+9999)/10000) }
