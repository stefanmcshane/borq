package openbao

import (
	"context"
	"fmt"
	"strings"

	openbaoapi "github.com/openbao/openbao/api/v2"
)

// EnsureKvV2 creates an isolated versioned mount without modifying existing engines.
func (c *Client) EnsureKvV2(ctx context.Context, mount string) error {
	mount = strings.Trim(mount, "/")
	if _, _, err := kvLocation(mount, "check"); err != nil {
		return err
	}
	mounts, err := c.cli.Sys().ListMountsWithContext(ctx)
	if err != nil {
		return kvRequestError(ctx, "list mounts", err)
	}
	if existing, exists := mounts[mount+"/"]; exists {
		if existing.Type != "kv" || existing.Options["version"] != "2" {
			return fmt.Errorf("openbao mount %s must use KV v2", mount)
		}
		return nil
	}
	if err := c.cli.Sys().MountWithContext(ctx, mount, &openbaoapi.MountInput{Type: "kv", Options: map[string]string{"version": "2"}}); err != nil {
		return kvRequestError(ctx, "enable KV v2", err)
	}
	return nil
}

// PutPolicy saves the explicit ACL used by a cluster's isolated vault token.
func (c *Client) PutPolicy(ctx context.Context, name, policy string) error {
	if name == "" || strings.ContainsAny(name, "/\\?#") || policy == "" {
		return fmt.Errorf("openbao policy requires a name and rules")
	}
	if err := c.cli.Sys().PutPolicyWithContext(ctx, name, policy); err != nil {
		return kvRequestError(ctx, "write policy", err)
	}
	return nil
}

// CreatePeriodicToken issues an orphan credential renewable independently of its issuer.
// Borq renews its 24-hour period while managing the registered cluster.
func (c *Client) CreatePeriodicToken(ctx context.Context, policy string) (string, error) {
	secret, err := c.cli.Auth().Token().CreateOrphanWithContext(ctx, &openbaoapi.TokenCreateRequest{
		Policies: []string{policy}, DisplayName: policy, NoDefaultPolicy: true, Period: "24h",
	})
	if err != nil {
		return "", kvRequestError(ctx, "create cluster token", err)
	}
	if secret == nil || secret.Auth == nil || secret.Auth.ClientToken == "" {
		return "", fmt.Errorf("openbao did not return a cluster token")
	}
	return secret.Auth.ClientToken, nil
}

// RenewToken renews an existing periodic credential without exposing its contents.
func (c *Client) RenewToken(ctx context.Context, token string) error {
	if _, err := c.cli.Auth().Token().RenewWithContext(ctx, token, 0); err != nil {
		return kvRequestError(ctx, "renew cluster token", err)
	}
	return nil
}
