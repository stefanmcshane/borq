// Package objectstore wraps the S3 protocol with explicit credentials and bounded reads.
package objectstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Credentials are transient material resolved from the management vault.
type Credentials struct {
	// AccessKey identifies the S3 principal.
	AccessKey string `json:"-"`
	// SecretKey signs S3 requests.
	SecretKey string `json:"-"`
	// SessionToken supports temporary S3 credentials.
	SessionToken string `json:"-"`
}

// Config selects one S3 bucket and its isolated root prefix.
type Config struct {
	// Endpoint is an HTTP(S) S3 endpoint, empty for AWS regional defaults.
	Endpoint string
	// Region selects the signing region.
	Region string
	// Bucket contains all objects accessed by this client.
	Bucket string
	// Prefix confines all relative object keys to one root.
	Prefix string
	// PathStyle enables bucket paths for compatible S3 services.
	PathStyle bool
	// Credentials are never serialized or persisted by this package.
	Credentials Credentials `json:"-"`
}

// Object contains catalogue metadata without downloading object bodies.
type Object struct {
	// Key is relative to Config.Prefix.
	Key string
	// Size is the content length in bytes.
	Size int64
	// LastModified is the service's recorded write time.
	LastModified time.Time
}

// Client owns the SDK transport, which requires shared pointer ownership.
type Client struct {
	client *s3.Client
	config Config
}

// New constructs an S3 client without using process environment or ambient credentials.
func New(config Config) (Client, error) {
	if config.Region == "" || config.Bucket == "" || strings.ContainsAny(config.Bucket, "/\\") || config.Credentials.AccessKey == "" || config.Credentials.SecretKey == "" {
		return Client{}, errors.New("S3 region, bucket, access key and secret key are required")
	}
	if config.Endpoint != "" {
		endpoint, err := url.Parse(config.Endpoint)
		if err != nil || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || (endpoint.Path != "" && endpoint.Path != "/") {
			return Client{}, errors.New("S3 endpoint must be an HTTP(S) origin without credentials")
		}
	}
	config.Prefix = strings.Trim(config.Prefix, "/")
	if _, err := scopedKey(config.Prefix, ""); err != nil {
		return Client{}, err
	}
	options := s3.Options{Region: config.Region, Credentials: credentials.NewStaticCredentialsProvider(config.Credentials.AccessKey, config.Credentials.SecretKey, config.Credentials.SessionToken), UsePathStyle: config.PathStyle, RetryMaxAttempts: 2}
	if config.Endpoint != "" {
		options.BaseEndpoint = aws.String(strings.TrimRight(config.Endpoint, "/"))
	}
	return Client{client: s3.New(options), config: config}, nil
}

// Validate proves list, write, read and delete permissions using a disposable object.
func (client Client) Validate(ctx context.Context) error {
	probeCtx, cancelProbe := context.WithTimeout(ctx, 30*time.Second)
	defer cancelProbe()
	prefix, err := scopedKey(client.config.Prefix, ".borq-validation/")
	if err != nil {
		return err
	}
	if _, err := client.client.ListObjectsV2(probeCtx, &s3.ListObjectsV2Input{Bucket: aws.String(client.config.Bucket), Prefix: aws.String(prefix), MaxKeys: aws.Int32(1)}); err != nil {
		return requestError(probeCtx, "list")
	}
	token := make([]byte, 16)
	if _, err := rand.Read(token); err != nil {
		return errors.New("generate S3 validation identity")
	}
	key := ".borq-validation/" + hex.EncodeToString(token)
	payload := []byte("borq object storage validation")
	if err := client.Put(ctx, key, payload); err != nil {
		return err
	}
	data, readErr := client.Read(ctx, key)
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	deleteErr := client.Delete(cleanupCtx, key)
	if readErr != nil {
		return readErr
	}
	if !bytes.Equal(data, payload) {
		return errors.New("S3 validation read did not match the written object")
	}
	return deleteErr
}

// List reads all keys below a relative prefix, including paginated responses.
func (client Client) List(ctx context.Context, prefix string) ([]Object, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	key, err := scopedKey(client.config.Prefix, prefix)
	if err != nil {
		return nil, err
	}
	if prefix == "" && key != "" {
		key += "/"
	}
	pages := s3.NewListObjectsV2Paginator(client.client, &s3.ListObjectsV2Input{Bucket: aws.String(client.config.Bucket), Prefix: aws.String(key)})
	objects := []Object{}
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, requestError(ctx, "list")
		}
		if len(objects)+len(page.Contents) > 100000 {
			return nil, errors.New("S3 listing exceeds 100000 objects; use ListPage to stream the archive")
		}
		for _, entry := range page.Contents {
			objects = append(objects, Object{Key: strings.TrimPrefix(aws.ToString(entry.Key), rootPrefix(client.config.Prefix)), Size: aws.ToInt64(entry.Size), LastModified: aws.ToTime(entry.LastModified)})
		}
	}
	return objects, nil
}

// Read downloads a metadata object, rejecting bodies larger than 16 MiB.
func (client Client) Read(ctx context.Context, key string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	key, err := scopedKey(client.config.Prefix, key)
	if err != nil {
		return nil, err
	}
	response, err := client.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(client.config.Bucket), Key: aws.String(key)})
	if err != nil {
		return nil, requestError(ctx, "read")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, (16<<20)+1))
	if err != nil {
		return nil, requestError(ctx, "read")
	}
	if len(data) > 16<<20 {
		return nil, errors.New("S3 metadata object exceeds 16 MiB")
	}
	return data, nil
}

// Put writes a small object beneath the configured root.
func (client Client) Put(ctx context.Context, key string, data []byte) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	key, err := scopedKey(client.config.Prefix, key)
	if err != nil {
		return err
	}
	_, err = client.client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(client.config.Bucket), Key: aws.String(key), Body: bytes.NewReader(data)})
	if err != nil {
		return requestError(ctx, "write")
	}
	return nil
}

// Delete removes one relative object, treating an absent key as success.
func (client Client) Delete(ctx context.Context, key string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	key, err := scopedKey(client.config.Prefix, key)
	if err != nil {
		return err
	}
	_, err = client.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(client.config.Bucket), Key: aws.String(key)})
	if err != nil {
		return requestError(ctx, "delete")
	}
	return nil
}

func rootPrefix(prefix string) string {
	if prefix == "" {
		return ""
	}
	return prefix + "/"
}
func scopedKey(prefix, key string) (string, error) {
	for _, value := range []string{prefix, key} {
		if strings.HasPrefix(value, "/") || strings.Contains(value, "\\") {
			return "", errors.New("S3 keys must be relative")
		}
		for _, segment := range strings.Split(value, "/") {
			if segment == "." || segment == ".." {
				return "", errors.New("S3 keys cannot traverse parent prefixes")
			}
		}
	}
	if key == "" {
		return prefix, nil
	}
	return rootPrefix(prefix) + key, nil
}
func requestError(ctx context.Context, action string) error {
	if ctx.Err() != nil {
		return fmt.Errorf("S3 %s: %w", action, ctx.Err())
	}
	return fmt.Errorf("S3 %s failed", action)
}

// ObjectPage supports bounded streaming of large archive catalogues.
type ObjectPage struct {
	// Objects contains at most the requested number of metadata entries.
	Objects []Object
	// Continuation is empty when the prefix has been completely listed.
	Continuation string
}

// ListPage lists at most 1000 objects and preserves S3's opaque continuation token.
func (client Client) ListPage(ctx context.Context, prefix, continuation string, limit int32) (ObjectPage, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if limit < 1 || limit > 1000 {
		return ObjectPage{}, errors.New("S3 list limit must be between 1 and 1000")
	}
	key, err := scopedKey(client.config.Prefix, prefix)
	if err != nil {
		return ObjectPage{}, err
	}
	if prefix == "" && key != "" {
		key += "/"
	}
	input := &s3.ListObjectsV2Input{Bucket: aws.String(client.config.Bucket), Prefix: aws.String(key), MaxKeys: aws.Int32(limit)}
	if continuation != "" {
		input.ContinuationToken = aws.String(continuation)
	}
	page, err := client.client.ListObjectsV2(ctx, input)
	if err != nil {
		return ObjectPage{}, requestError(ctx, "list")
	}
	result := ObjectPage{Objects: []Object{}, Continuation: aws.ToString(page.NextContinuationToken)}
	for _, entry := range page.Contents {
		result.Objects = append(result.Objects, Object{Key: strings.TrimPrefix(aws.ToString(entry.Key), rootPrefix(client.config.Prefix)), Size: aws.ToInt64(entry.Size), LastModified: aws.ToTime(entry.LastModified)})
	}
	return result, nil
}
