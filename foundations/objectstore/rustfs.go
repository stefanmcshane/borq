package objectstore

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// RustfsCredentials creates a bucket or prefix scoped account using the RustFS admin API.
// Its parent credentials remain in management; derived keys cannot administer RustFS.
func RustfsCredentials(ctx context.Context, config Config) (Credentials, error) {
	return rustfsCredentials(ctx, config, false)
}

// RustfsReadCredentials limits recovery workers to reading the selected archive.
func RustfsReadCredentials(ctx context.Context, config Config) (Credentials, error) {
	return rustfsCredentials(ctx, config, true)
}

func rustfsCredentials(ctx context.Context, config Config, readOnly bool) (Credentials, error) {
	if err := config.Validate(); err != nil {
		return Credentials{}, err
	}
	if config.Endpoint == "" || strings.ContainsAny(config.Prefix, "*?") || config.Credentials.SecretKey == "" || config.Credentials.SessionToken != "" {
		return Credentials{}, fmt.Errorf("managed backups require RustFS with long-term management credentials")
	}
	// Stable secret-derived identities make retries idempotent without storing
	// upload secrets in plans or creating an account on every deployment.
	derive := func(purpose string) string {
		if readOnly {
			purpose = "read/" + purpose
		}
		mac := hmac.New(sha256.New, []byte(config.Credentials.SecretKey))
		_, _ = mac.Write([]byte("borq/storage/" + purpose + "/" + config.Bucket + "/" + strings.Trim(config.Prefix, "/")))
		return hex.EncodeToString(mac.Sum(nil))
	}
	keys := Credentials{AccessKey: "bq" + derive("access")[:30], SecretKey: derive("secret")[:40]}
	root := "arn:aws:s3:::" + config.Bucket
	prefix := strings.Trim(config.Prefix, "/")
	listing := map[string]any{"Effect": "Allow", "Action": []string{"s3:ListBucket"}, "Resource": []string{root}}
	objects := root + "/*"
	if prefix != "" {
		listing["Condition"] = map[string]any{"StringLike": map[string]any{"s3:prefix": []string{prefix, prefix + "/*"}}}
		objects = root + "/" + prefix + "/*"
	}
	actions := []string{"s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:AbortMultipartUpload", "s3:ListMultipartUploadParts"}
	if readOnly {
		actions = []string{"s3:GetObject"}
	}
	policy := map[string]any{"Version": "2012-10-17", "Statement": []any{
		listing,
		map[string]any{"Effect": "Allow", "Action": []string{"s3:GetBucketLocation"}, "Resource": []string{root}},
		map[string]any{"Effect": "Allow", "Action": actions, "Resource": []string{objects}},
	}}
	status, err := rustfsAdmin(ctx, config, http.MethodGet, "info-service-account?accessKey="+url.QueryEscape(keys.AccessKey), nil)
	if err != nil {
		return Credentials{}, err
	}
	method, operation := http.MethodPut, "add-service-account"
	body := map[string]any{"accessKey": keys.AccessKey, "secretKey": keys.SecretKey, "policy": policy, "name": "borq-backup"}
	if status == http.StatusOK {
		method, operation = http.MethodPost, "update-service-account?accessKey="+url.QueryEscape(keys.AccessKey)
		body = map[string]any{"newSecretKey": keys.SecretKey, "newPolicy": policy, "newStatus": "on"}
	} else if status != http.StatusNotFound {
		return Credentials{}, fmt.Errorf("RustFS backup account lookup failed (status %d)", status)
	}
	status, err = rustfsAdmin(ctx, config, method, operation, body)
	if err != nil {
		return Credentials{}, err
	}
	if status != http.StatusOK && !(method == http.MethodPost && status == http.StatusNoContent) {
		return Credentials{}, fmt.Errorf("RustFS scoped backup account update failed (status %d)", status)
	}
	return keys, nil
}

func rustfsAdmin(ctx context.Context, config Config, method, operation string, body map[string]any) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var data []byte
	if body != nil {
		data, _ = json.Marshal(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(config.Endpoint, "/")+"/rustfs/admin/v3/"+operation, bytes.NewReader(data))
	if err != nil {
		return 0, fmt.Errorf("construct RustFS backup account request")
	}
	request.Header.Set("Content-Type", "application/json")
	digest := sha256.Sum256(data)
	payloadHash := hex.EncodeToString(digest[:])
	request.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if err := v4.NewSigner().SignHTTP(ctx, aws.Credentials{AccessKeyID: config.Credentials.AccessKey, SecretAccessKey: config.Credentials.SecretKey}, request, payloadHash, "s3", config.Region, time.Now()); err != nil {
		return 0, fmt.Errorf("sign RustFS backup account request")
	}
	transport := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := transport.Do(request)
	if err != nil {
		return 0, fmt.Errorf("RustFS backup account request failed")
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	return response.StatusCode, nil
}
