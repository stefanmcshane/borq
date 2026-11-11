// Package openbao provides a thin wrapper around the official OpenBao
// Go client (github.com/openbao/openbao/api/v2) for initialization, unsealing,
// and versioned key-value storage.
package openbao

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	openbaoapi "github.com/openbao/openbao/api/v2"
)

// SealStatus describes whether an OpenBao instance has been initialized and
// whether it is currently sealed.
type SealStatus struct {
	Initialized bool
	Sealed      bool
}

// InitResult contains the credentials produced by initializing an OpenBao
// server for the first time.
type InitResult struct {
	RootToken     string
	UnsealKeysB64 []string
}

// NewClientInput configures a new OpenBao API client.
type NewClientInput struct {
	// Address is the full URL of the OpenBao instance
	// (e.g. "http://127.0.0.1:8200").
	Address string

	// Token authenticates requests explicitly; an empty value sends no token.
	Token string

	// HttpClient supplies a custom transport, including Kubernetes service proxy
	// authentication. Its zero value uses the SDK's default HTTP transport.
	HttpClient http.Client

	// RequestTimeout is the per-request timeout. Zero means no timeout.
	RequestTimeout time.Duration
}

// Client is a lightweight OpenBao API client that wraps the official SDK.
type Client struct {
	cli *openbaoapi.Client
}

// NewClient creates a new OpenBao API client connected to the given address.
func NewClient(input NewClientInput) (*Client, error) {
	address := input.Address
	if address == "" {
		address = "https://127.0.0.1:8200"
	}
	if input.HttpClient.CheckRedirect == nil {
		// Let the SDK enforce its own redirect rules, including TLS downgrades.
		input.HttpClient.CheckRedirect = func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}
	// The SDK retains the HTTP client pointer for subsequent requests.
	config := &openbaoapi.Config{
		Address:    address,
		HttpClient: &input.HttpClient,
		Timeout:    input.RequestTimeout,
	}
	cli, err := openbaoapi.NewClient(config)
	if err != nil {
		return nil, fmt.Errorf("create openbao client: invalid client configuration")
	}
	// Override the SDK's ambient token and namespace so callers own credentials.
	cli.SetToken(input.Token)
	cli.ClearNamespace()
	return &Client{cli: cli}, nil
}

// Status returns the current seal status of the OpenBao server.
func (c *Client) Status(ctx context.Context) (*SealStatus, error) {
	resp, err := c.cli.Sys().SealStatusWithContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("openbao seal-status: %w", err)
	}
	return &SealStatus{
		Initialized: resp.Initialized,
		Sealed:      resp.Sealed,
	}, nil
}

// Init initializes a new OpenBao server with a single unseal key and
// returns the root token and unseal keys.
func (c *Client) Init(ctx context.Context) (*InitResult, error) {
	request := &openbaoapi.InitRequest{
		SecretShares:    1,
		SecretThreshold: 1,
	}
	resp, err := c.cli.Sys().InitWithContext(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("openbao init: %w", err)
	}

	result := &InitResult{
		RootToken:     resp.RootToken,
		UnsealKeysB64: append([]string(nil), resp.KeysB64...),
	}
	return result, nil
}

// Unseal submits a single unseal key to the OpenBao server. If the key is
// correct and the threshold is met, the server transitions to an unsealed
// state.
func (c *Client) Unseal(ctx context.Context, key string) error {
	_, err := c.cli.Sys().UnsealWithContext(ctx, key)
	if err != nil {
		return fmt.Errorf("openbao unseal: %w", err)
	}
	return nil
}

// WaitForReady polls the OpenBao server until it responds to a seal-status
// request or the timeout expires. The pollInterval controls how frequently
// the server is checked.
func (c *Client) WaitForReady(ctx context.Context, pollInterval, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for openbao ready: %w", ctx.Err())
		default:
		}

		_, err := c.Status(ctx)
		if err == nil {
			return nil
		}

		time.Sleep(pollInterval)
	}

	return fmt.Errorf("openbao not reachable within %s", timeout)
}

// WaitForActive waits for the unsealed local instance to finish Raft leader activation before state-changing requests.
func (client *Client) WaitForActive(ctx context.Context, pollInterval, timeout time.Duration) error {
	if pollInterval <= 0 || timeout <= 0 {
		return errors.New("OpenBao activation wait requires positive durations")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		leader, err := client.cli.Sys().LeaderWithContext(ctx)
		if err == nil && (!leader.HAEnabled || leader.IsSelf) {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("OpenBao did not become active before the activation deadline")
		case <-ticker.C:
		}
	}
}
