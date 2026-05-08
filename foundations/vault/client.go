// Package vault provides a thin wrapper around the official HashiCorp Vault
// Go client (github.com/hashicorp/vault-client-go), exposing only the
// subset needed for server initialization and unsealing.
package vault

import (
	"context"
	"fmt"
	"time"

	vaultclient "github.com/hashicorp/vault-client-go"
	"github.com/hashicorp/vault-client-go/schema"
)

// SealStatus describes whether a Vault instance has been initialized and
// whether it is currently sealed.
type SealStatus struct {
	Initialized bool
	Sealed      bool
}

// InitResult contains the credentials produced by initializing a Vault
// server for the first time.
type InitResult struct {
	RootToken     string
	UnsealKeysB64 []string
}

// NewClientInput configures a new Vault API client.
type NewClientInput struct {
	// Address is the full URL of the Vault instance
	// (e.g. "http://127.0.0.1:8200").
	Address string

	// RequestTimeout is the per-request timeout. Zero means no timeout.
	RequestTimeout time.Duration
}

// Client is a lightweight Vault API client that wraps the official SDK.
type Client struct {
	cli *vaultclient.Client
}

// NewClient creates a new Vault API client connected to the given address.
func NewClient(input NewClientInput) (*Client, error) {
	opts := []vaultclient.ClientOption{
		vaultclient.WithAddress(input.Address),
		vaultclient.WithRequestTimeout(input.RequestTimeout),
	}
	cli, err := vaultclient.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("create vault client: %w", err)
	}
	return &Client{cli: cli}, nil
}

// Status returns the current seal status of the Vault server.
func (c *Client) Status(ctx context.Context) (*SealStatus, error) {
	resp, err := c.cli.System.SealStatus(ctx)
	if err != nil {
		return nil, fmt.Errorf("vault seal-status: %w", err)
	}
	return &SealStatus{
		Initialized: resp.Data.Initialized,
		Sealed:      resp.Data.Sealed,
	}, nil
}

// Init initializes a new Vault server with a single unseal key and
// returns the root token and unseal keys.
func (c *Client) Init(ctx context.Context) (*InitResult, error) {
	request := schema.InitializeRequest{
		SecretShares:    1,
		SecretThreshold: 1,
	}
	resp, err := c.cli.System.Initialize(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("vault init: %w", err)
	}

	result := &InitResult{}
	if rootToken, ok := resp.Data["root_token"].(string); ok {
		result.RootToken = rootToken
	}
	if keysRaw, ok := resp.Data["keys_base64"].([]any); ok {
		for _, k := range keysRaw {
			if s, ok := k.(string); ok {
				result.UnsealKeysB64 = append(result.UnsealKeysB64, s)
			}
		}
	}
	return result, nil
}

// Unseal submits a single unseal key to the Vault server. If the key is
// correct and the threshold is met, the server transitions to an unsealed
// state.
func (c *Client) Unseal(ctx context.Context, key string) error {
	request := schema.UnsealRequest{Key: key}
	_, err := c.cli.System.Unseal(ctx, request)
	if err != nil {
		return fmt.Errorf("vault unseal: %w", err)
	}
	return nil
}

// WaitForReady polls the Vault server until it responds to a seal-status
// request or the timeout expires. The pollInterval controls how frequently
// the server is checked.
func (c *Client) WaitForReady(ctx context.Context, pollInterval, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for vault ready: %w", ctx.Err())
		default:
		}

		_, err := c.Status(ctx)
		if err == nil {
			return nil
		}

		time.Sleep(pollInterval)
	}

	return fmt.Errorf("vault not reachable within %s", timeout)
}
