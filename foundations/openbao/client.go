// Package openbao provides a thin wrapper around the official OpenBao
// Go client (github.com/openbao/openbao/api/v2), exposing only the
// subset needed for server initialization and unsealing.
package openbao

import (
	"context"
	"fmt"
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

	// RequestTimeout is the per-request timeout. Zero means no timeout.
	RequestTimeout time.Duration
}

// Client is a lightweight OpenBao API client that wraps the official SDK.
type Client struct {
	cli *openbaoapi.Client
}

// NewClient creates a new OpenBao API client connected to the given address.
func NewClient(input NewClientInput) (*Client, error) {
	config := openbaoapi.DefaultConfig()
	cli, err := openbaoapi.NewClient(config)
	if err != nil {
		return nil, fmt.Errorf("create openbao client: %w", err)
	}
	if input.Address != "" {
		cli.SetAddress(input.Address)
	}
	if input.RequestTimeout > 0 {
		cli.SetClientTimeout(input.RequestTimeout)
	}
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
