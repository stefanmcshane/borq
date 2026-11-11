package openbao

import (
	"context"
	"errors"
	"io"
)

// SaveRaftSnapshot streams a complete native Raft snapshot through the official SDK.
func (client *Client) SaveRaftSnapshot(ctx context.Context, output io.Writer) error {
	if err := client.cli.Sys().RaftSnapshotWithContext(ctx, output); err != nil {
		return errors.New("save native OpenBao Raft snapshot")
	}
	return nil
}

// RestoreRaftSnapshot restores native encrypted state into an explicitly selected recovery instance.
// Force permits the fresh instance's temporary seal keys to differ from the original keys.
func (client *Client) RestoreRaftSnapshot(ctx context.Context, input io.Reader, force bool) error {
	if err := client.cli.Sys().RaftSnapshotRestoreWithContext(ctx, input, force); err != nil {
		return errors.New("restore native OpenBao Raft snapshot")
	}
	return nil
}

// CheckToken verifies that independently supplied administrative access survived native recovery.
func (client *Client) CheckToken(ctx context.Context) error {
	if _, err := client.cli.Auth().Token().LookupSelfWithContext(ctx); err != nil {
		return errors.New("retained OpenBao token does not authenticate restored state")
	}
	return nil
}
