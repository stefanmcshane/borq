package apps

import (
	"context"
	"errors"

	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/business/server/sqliteworkload"
	"github.com/stefanmcshane/borq/foundations/openbao"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

func (manager Manager) sqliteArchiveKey(ctx context.Context, space Space, application string) (sqliteworkload.ArchiveKey, error) {
	key := space.ID + "/" + application
	reference := "borq/sqlite-archives/" + key
	retained, record, err := loadDocument[string](ctx, manager.database, "sqlite-archive-key", key)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return sqliteworkload.ArchiveKey{}, err
	}
	if record.Revision != 0 && retained != reference {
		return sqliteworkload.ArchiveKey{}, errors.New("SQLite archive encryption ownership changed")
	}
	vault, err := platform.ManagementVault(ctx, manager.runtime.managementKubeconfigPath, manager.runtime.platformSettings.Namespace)
	if err != nil {
		return sqliteworkload.ArchiveKey{}, errors.New("SQLite archive encryption secret storage is unavailable")
	}
	entry, err := vault.ReadKv(ctx, "secret", reference, 1)
	if errors.Is(err, openbao.ErrNotFound) {
		// Accepted backups and the retained reference are evidence that a key already
		// existed. Never hide its loss by encrypting a new archive with another key.
		required := record.Revision != 0
		stored, _, readErr := loadDocument[spaceRecord](ctx, manager.database, "space", spaceKey(space.Organization, space.Name))
		if readErr != nil {
			return sqliteworkload.ArchiveKey{}, readErr
		}
		for _, previous := range stored.Space.Configuration.Apps {
			required = required || previous.Name == application && previous.Sqlite.Backups
		}
		if required {
			return sqliteworkload.ArchiveKey{}, errors.New("SQLite archive encryption key is missing; restore its original vault entry before retrying")
		}
		generated, generationErr := sqliteworkload.NewArchiveKey()
		if generationErr != nil {
			return sqliteworkload.ArchiveKey{}, generationErr
		}
		entry, err = vault.WriteKv(ctx, "secret", reference, openbao.WriteKvInput{Values: map[string]string{"ageIdentity": generated.Identity, "ageRecipient": generated.Recipient}, CheckVersion: 0})
		if errors.Is(err, openbao.ErrConflict) {
			entry, err = vault.ReadKv(ctx, "secret", reference, 1)
		}
	}
	if err != nil {
		return sqliteworkload.ArchiveKey{}, errors.New("SQLite archive encryption key is unavailable")
	}
	archive := sqliteworkload.ArchiveKey{Identity: entry.Values["ageIdentity"], Recipient: entry.Values["ageRecipient"]}
	if err := archive.Validate(); err != nil {
		return sqliteworkload.ArchiveKey{}, err
	}
	if record.Revision == 0 {
		write, err := documentWrite("sqlite-archive-key", key, reference, 0)
		if err != nil {
			return sqliteworkload.ArchiveKey{}, err
		}
		if err := manager.database.Commit(ctx, []sqlite.Write{write}); err != nil && !errors.Is(err, sqlite.ErrConflict) {
			return sqliteworkload.ArchiveKey{}, err
		}
	}
	return archive, nil
}
