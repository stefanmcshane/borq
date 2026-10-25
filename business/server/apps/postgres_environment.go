package apps

import (
	"context"
	"errors"
	"fmt"
	"maps"

	"github.com/stefanmcshane/borq/foundations/openbao"
	"github.com/stefanmcshane/borq/foundations/postgres"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

func postgresEnvironmentName(stored postgresRecord) string {
	return "postgres-" + stored.Postgres.Name + "-" + stored.ID
}

func (manager Manager) claimPostgresEnvironment(ctx context.Context, stored postgresRecord, target Space) (postgresRecord, []sqlite.Write, error) {
	name := postgresEnvironmentName(stored)
	if err := validateConfigName(name); err != nil {
		return stored, nil, err
	}
	if stored.Postgres.EnvironmentGroup != "" && stored.Postgres.EnvironmentGroup != name {
		return stored, nil, fmt.Errorf("%w: PostgreSQL environment group identity changed", ErrConflict)
	}
	path := environmentGroupPath(target.ID, name)
	existing, _, err := loadDocument[environmentGroupRecord](ctx, manager.database, "environment-group", target.ID+"/"+name)
	if err == nil {
		if existing.PostgresID != stored.ID || existing.PostgresName != stored.Postgres.Name || existing.VaultPath != path {
			return stored, nil, fmt.Errorf("%w: PostgreSQL environment group is already reserved", ErrConflict)
		}
		stored.Postgres.EnvironmentGroup = name
		return stored, nil, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return stored, nil, err
	}
	_, client, err := manager.environmentGroupConnection(ctx, EnvironmentGroupTarget{Organization: stored.Organization, Space: stored.Space})
	if err != nil {
		return stored, nil, err
	}
	vault, mount, err := openEnvironmentVault(ctx, client)
	if err != nil {
		return stored, nil, err
	}
	for _, candidate := range []string{path, legacyEnvironmentGroupPath(target.ID, name)} {
		_, err := vault.ReadKvMetadata(ctx, mount, candidate)
		if err == nil {
			return stored, nil, fmt.Errorf("%w: PostgreSQL environment group has existing unowned history", ErrConflict)
		}
		if !errors.Is(err, openbao.ErrNotFound) {
			return stored, nil, err
		}
	}
	claim := environmentGroupRecord{SpaceID: target.ID, Name: name, VaultPath: path, PostgresID: stored.ID, PostgresName: stored.Postgres.Name}
	write, err := documentWrite("environment-group", target.ID+"/"+name, claim, 0)
	if err != nil {
		return stored, nil, err
	}
	stored.Postgres.EnvironmentGroup = name
	return stored, []sqlite.Write{write}, nil
}

func (manager Manager) postgresEnvironmentRetention(ctx context.Context, stored postgresRecord) error {
	target, client, err := manager.environmentGroupConnection(ctx, EnvironmentGroupTarget{Organization: stored.Organization, Space: stored.Space})
	if err != nil {
		return err
	}
	vault, mount, err := openEnvironmentVault(ctx, client)
	if err != nil {
		return err
	}
	return vault.SetKvRetention(ctx, mount, environmentGroupPath(target.ID, stored.Postgres.EnvironmentGroup), 1)
}

func (manager Manager) postgresEnvironmentReady(ctx context.Context, stored postgresRecord, client postgres.Client, input postgres.Input) (bool, error) {
	ready, err := client.EnvironmentExportReady(ctx, input)
	if err != nil || !ready {
		return false, err
	}
	expected, err := client.Environment(ctx, input)
	if err != nil {
		return false, err
	}
	engine, err := manager.postgresVault(ctx)
	if err != nil {
		return false, err
	}
	credentials, err := engine.ReadStaticCredentials(ctx, input.Credentials.Mount, input.Credentials.Role)
	if err != nil {
		return false, err
	}
	if expected["PGUSER"] != credentials.Username || expected["PGPASSWORD"] != credentials.Password {
		return false, nil
	}
	_, targetClient, err := manager.environmentGroupConnection(ctx, EnvironmentGroupTarget{Organization: stored.Organization, Space: stored.Space})
	if err != nil {
		return false, err
	}
	vault, mount, err := openEnvironmentVault(ctx, targetClient)
	if err != nil {
		return false, err
	}
	entry, err := vault.ReadKv(ctx, mount, input.EnvironmentExport.VaultPath, 0)
	if errors.Is(err, openbao.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return maps.Equal(expected, entry.Values), nil
}

type postgresEnvironmentFailure struct {
	stage string
	err   error
}

func (failure postgresEnvironmentFailure) Error() string { return failure.stage }
func (failure postgresEnvironmentFailure) Unwrap() error { return failure.err }

func postgresEnvironmentStep(stage string, err error) error {
	if err == nil {
		return nil
	}
	return postgresEnvironmentFailure{stage: stage, err: err}
}

func (manager Manager) maintainPostgresEnvironment(ctx context.Context, key, identity string) error {
	manager.runtime.storageMutex.Lock()
	defer manager.runtime.storageMutex.Unlock()
	stored, record, err := loadDocument[postgresRecord](ctx, manager.database, "postgres", key)
	if err != nil {
		return err
	}
	if stored.ID != identity || stored.Postgres.State == "removed" || stored.Postgres.State == "deleting" || stored.ActiveOperation != "" {
		return nil
	}
	deleting, err := manager.postgresDeletionAccepted(ctx, stored)
	if err != nil || deleting {
		return err
	}
	target, err := manager.GetSpace(ctx, stored.Organization, stored.Space)
	if err != nil {
		return err
	}
	previousName := stored.Postgres.EnvironmentGroup
	stored, writes, err := manager.claimPostgresEnvironment(ctx, stored, target)
	if err != nil {
		return err
	}
	if previousName != stored.Postgres.EnvironmentGroup {
		write, err := documentWrite("postgres", key, stored, record.Revision)
		if err != nil {
			return err
		}
		writes = append(writes, write)
	}
	if len(writes) > 0 {
		if err := manager.database.Commit(ctx, writes); err != nil {
			return translateStoreError(err)
		}
	}
	if err := manager.postgresEnvironmentRetention(ctx, stored); err != nil {
		return postgresEnvironmentStep("configure credential history retention", err)
	}
	client, input, err := manager.postgresClient(ctx, stored, false)
	if err != nil {
		return postgresEnvironmentStep("connect to database cluster", err)
	}
	observed, err := client.Observe(ctx, input)
	if err != nil || !observed.Ready {
		return postgresEnvironmentStep("observe database readiness", err)
	}
	input, err = manager.reconcilePostgresCredentials(ctx, stored, client, input)
	if err != nil {
		return postgresEnvironmentStep("reconcile OpenBao credentials", err)
	}
	ready, err := manager.postgresEnvironmentReady(ctx, stored, client, input)
	if err != nil || !ready {
		return postgresEnvironmentStep("verify credential delivery", err)
	}
	_, err = manager.refreshPostgresConsumers(ctx, stored)
	return postgresEnvironmentStep("refresh application credentials", err)
}

func safePostgresEnvironmentError(err error) string {
	var failure postgresEnvironmentFailure
	if errors.As(err, &failure) {
		return failure.stage + ": " + safePostgresEnvironmentError(failure.err)
	}
	var credentialFailure postgresCredentialError
	if errors.As(err, &credentialFailure) {
		return credentialFailure.message
	}
	switch {
	case errors.Is(err, ErrConflict):
		return "environment ownership conflict"
	case errors.Is(err, context.DeadlineExceeded):
		return "dependency request timed out"
	case errors.Is(err, context.Canceled):
		return "manager stopped"
	case errors.Is(err, ErrNotFound):
		return "accepted dependency is missing"
	default:
		return "inspect the owned application Secret, PushSecret, and target OpenBao store"
	}
}

func (manager Manager) postgresDeletionAccepted(ctx context.Context, stored postgresRecord) (bool, error) {
	if stored.Postgres.Operation == "" {
		return false, nil
	}
	task, _, err := loadDocument[postgresTask](ctx, manager.database, "postgres-task", stored.Postgres.Operation)
	if err != nil {
		return false, err
	}
	return task.Action == "delete", nil
}
