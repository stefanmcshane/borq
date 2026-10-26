package managementbackup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/stefanmcshane/borq/foundations/objectstore"
	"github.com/stefanmcshane/borq/foundations/openbao"
)

// OpenBaoRecovery contains independently retained credentials for a selected recovery instance.
type OpenBaoRecovery struct {
	// Address selects the fresh Raft instance, independently of the lost management database.
	Address string `json:"address"`
	// BootstrapToken authenticates an already initialized fresh target. Empty initializes a fresh target directly.
	BootstrapToken string `json:"bootstrapToken,omitempty"`
	// OriginalToken is the retained original administrative token, needed after state restoration.
	OriginalToken string `json:"originalToken"`
	// UnsealKeys contains the independently retained original unseal material.
	UnsealKeys []string `json:"unsealKeys"`
}

// RestoreInput selects one completed artifact and its independent private recovery material.
type RestoreInput struct {
	// Storage selects local or rehydrated S3 with explicitly supplied credentials.
	Storage objectstore.Config
	// Kind and ID select one independent component artifact.
	Kind, ID string
	// Identity is retained outside Borq and is used only for authenticated decryption.
	Identity string
	// DataDir is a fresh SQLite destination, unused for OpenBao.
	DataDir string
	// OpenBao selects native Raft recovery without any dependency on the original vault.
	OpenBao OpenBaoRecovery
}

// Restore validates complete encrypted bytes before publishing files or invoking a native Raft restore.
func Restore(ctx context.Context, input RestoreInput) error {
	if !ValidKind(input.Kind) {
		return errors.New("native restore kind must be sqlite or openbao")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Hour)
	defer cancel()
	directory, err := os.MkdirTemp("", "borq-native-restore-")
	if err != nil {
		return errors.New("create private native recovery workspace")
	}
	defer os.RemoveAll(directory)
	encrypted := filepath.Join(directory, "payload.age")
	decrypted := filepath.Join(directory, "payload")
	if _, err := Download(ctx, input.Storage, input.Kind, input.ID, encrypted); err != nil {
		return err
	}
	if err := Decrypt(ctx, encrypted, decrypted, input.Identity); err != nil {
		return err
	}
	if input.Kind == "sqlite" {
		return RestoreSQLite(ctx, decrypted, input.DataDir)
	}
	return restoreOpenBao(ctx, input.OpenBao, decrypted)
}
func restoreOpenBao(ctx context.Context, recovery OpenBaoRecovery, file string) error {
	if recovery.Address == "" || recovery.OriginalToken == "" || len(recovery.UnsealKeys) == 0 {
		return errors.New("OpenBao recovery requires an explicit target, original administrative token and retained unseal material")
	}
	client, err := openbao.NewClient(openbao.NewClientInput{Address: recovery.Address, Token: recovery.BootstrapToken, RequestTimeout: 30 * time.Minute})
	if err != nil {
		return errors.New("open selected OpenBao recovery instance")
	}
	status, err := client.Status(ctx)
	if err != nil {
		return errors.New("inspect selected OpenBao recovery instance")
	}
	if !status.Initialized {
		initialized, err := client.Init(ctx)
		if err != nil {
			return errors.New("initialize fresh OpenBao recovery instance")
		}
		for _, key := range initialized.UnsealKeysB64 {
			if err := client.Unseal(ctx, key); err != nil {
				return errors.New("unseal fresh OpenBao recovery instance")
			}
		}
		client, err = openbao.NewClient(openbao.NewClientInput{Address: recovery.Address, Token: initialized.RootToken, RequestTimeout: 30 * time.Minute})
		if err != nil {
			return errors.New("authenticate fresh OpenBao recovery instance")
		}
	} else if status.Sealed {
		return errors.New("initialized recovery instance must be unsealed with its temporary keys before native restore")
	}
	if err := client.WaitForActive(ctx, time.Second, 2*time.Minute); err != nil {
		return err
	}
	snapshot, err := os.Open(file)
	if err != nil {
		return errors.New("open authenticated native Raft snapshot")
	}
	defer snapshot.Close()
	if err := client.RestoreRaftSnapshot(ctx, snapshot, true); err != nil {
		return err
	}
	original, err := openbao.NewClient(openbao.NewClientInput{Address: recovery.Address, Token: recovery.OriginalToken, RequestTimeout: 30 * time.Second})
	if err != nil {
		return errors.New("authenticate restored OpenBao")
	}
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		status, err := original.Status(ctx)
		if err == nil && status.Initialized {
			if !status.Sealed {
				if original.CheckToken(ctx) == nil {
					return nil
				}
			}
			for _, key := range recovery.UnsealKeys {
				if err := original.Unseal(ctx, key); err != nil {
					return errors.New("restore requires the original OpenBao unseal material")
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return errors.New("restored OpenBao did not become unsealed with the retained original material")
}
