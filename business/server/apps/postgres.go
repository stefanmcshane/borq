package apps

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/stefanmcshane/borq/foundations/objectstore"
	"github.com/stefanmcshane/borq/foundations/openbao"
	"github.com/stefanmcshane/borq/foundations/sqlite"
	"k8s.io/apimachinery/pkg/api/resource"
)

type postgresRecord struct {
	Postgres        Postgres `json:"postgres"`
	Organization    string   `json:"organization"`
	Space           string   `json:"space"`
	Cluster         string   `json:"cluster"`
	ID              string   `json:"id"`
	ArchiveID       string   `json:"archiveId"`
	CredentialsRef  string   `json:"credentialsRef"`
	ActiveOperation string   `json:"activeOperation"`
	ActiveCheck     string   `json:"activeCheck,omitempty"`
}

type postgresArchive struct {
	ID             string        `json:"id"`
	Organization   string        `json:"organization"`
	OrganizationID string        `json:"organizationId"`
	Space          string        `json:"space"`
	Name           string        `json:"name"`
	ResourceID     string        `json:"resourceId"`
	Endpoint       string        `json:"endpoint"`
	Region         string        `json:"region"`
	Bucket         string        `json:"bucket"`
	Prefix         string        `json:"prefix"`
	ServerName     string        `json:"serverName"`
	PathStyle      bool          `json:"pathStyle"`
	CredentialsRef string        `json:"credentialsRef"`
	MajorVersion   int           `json:"majorVersion"`
	Specification  PostgresInput `json:"specification"`
	CreatedAt      time.Time     `json:"createdAt"`
}

type postgresTask struct {
	Key               string    `json:"key"`
	Action            string    `json:"action"`
	Deadline          time.Time `json:"deadline"`
	NextRetry         time.Time `json:"nextRetry,omitempty"`
	SourceArchive     string    `json:"sourceArchive,omitempty"`
	BackupID          string    `json:"backupId,omitempty"`
	Fingerprint       string    `json:"fingerprint,omitempty"`
	RecoveryTarget    time.Time `json:"recoveryTarget,omitempty"`
	CheckID           string    `json:"checkId,omitempty"`
	CheckName         string    `json:"checkName,omitempty"`
	CheckArchive      string    `json:"checkArchive,omitempty"`
	CheckJobUID       string    `json:"checkJobUid,omitempty"`
	Verified          bool      `json:"verified,omitempty"`
	SnapshotStatus    string    `json:"snapshotStatus,omitempty"`
	Cleanup           string    `json:"cleanup,omitempty"`
	VerificationError string    `json:"verificationError,omitempty"`
}

func normalizePostgres(input PostgresInput) (PostgresInput, error) {
	if input.Instances == 0 {
		input.Instances = 1
	}
	if input.Instances < 1 || input.Instances > 16 {
		return input, fmt.Errorf("%w: PostgreSQL instances must be between 1 and 16", ErrInvalid)
	}
	if input.Storage == "" {
		input.Storage = "1Gi"
	}
	capacity, err := resource.ParseQuantity(input.Storage)
	if err != nil || capacity.Sign() <= 0 {
		return input, fmt.Errorf("%w: PostgreSQL storage must be a positive Kubernetes quantity", ErrInvalid)
	}
	if input.Image == "" {
		input.Image = "ghcr.io/cloudnative-pg/postgresql:18.6-system-trixie"
	}
	if postgresMajor(input.Image) == 0 {
		return input, fmt.Errorf("%w: PostgreSQL image must pin a supported 14–18 major and exact minor tag", ErrInvalid)
	}
	if input.Database == "" {
		input.Database = "app"
	}
	if input.Username == "" {
		input.Username = "app"
	}
	identifier := regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)
	if !identifier.MatchString(input.Database) || !identifier.MatchString(input.Username) || input.Username == "postgres" {
		return input, fmt.Errorf("%w: database and application username must be lowercase SQL identifiers; postgres is reserved", ErrInvalid)
	}
	if input.Backup.Schedule == "" {
		input.Backup.Schedule = "0 0 * * *"
	}
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	if _, err := parser.Parse(input.Backup.Schedule); err != nil {
		return input, fmt.Errorf("%w: full backup schedule requires five cron fields", ErrInvalid)
	}
	if input.Backup.RecoveryWindowDays == 0 {
		input.Backup.RecoveryWindowDays = 30
	}
	if input.Backup.RecoveryWindowDays < 1 || input.Backup.RecoveryWindowDays > 3650 {
		return input, fmt.Errorf("%w: recovery window must be 1–3650 days", ErrInvalid)
	}
	if input.Snapshots == "" {
		input.Snapshots = "preferred"
	}
	if input.Snapshots != "preferred" && input.Snapshots != "required" && input.Snapshots != "disabled" {
		return input, fmt.Errorf("%w: snapshots must be preferred, required, or disabled", ErrInvalid)
	}
	if input.SnapshotSchedule == "" {
		input.SnapshotSchedule = "0 * * * *"
	}
	if _, err := parser.Parse(input.SnapshotSchedule); err != nil {
		return input, fmt.Errorf("%w: snapshot schedule requires five cron fields", ErrInvalid)
	}
	if input.SnapshotRetentionHours == 0 {
		input.SnapshotRetentionHours = 24
	}
	if input.SnapshotRetentionHours < 1 || input.SnapshotRetentionHours > 8760 {
		return input, fmt.Errorf("%w: snapshot retention must be 1–8760 hours", ErrInvalid)
	}
	if input.RestoreCheck.Schedule == "" {
		input.RestoreCheck.Schedule = "0 3 * * 0"
	}
	if _, err := cron.ParseStandard(input.RestoreCheck.Schedule); err != nil {
		return input, fmt.Errorf("%w: restore check schedule requires five cron fields", ErrInvalid)
	}
	if input.RestoreCheck.TimeoutSeconds == 0 {
		input.RestoreCheck.TimeoutSeconds = 1800
	}
	if input.RestoreCheck.TimeoutSeconds < 60 || input.RestoreCheck.TimeoutSeconds > 86400 {
		return input, fmt.Errorf("%w: restore check timeout must be 60–86400 seconds", ErrInvalid)
	}
	for _, resources := range []PostgresResources{input.Resources, input.RestoreCheck.Resources} {
		for _, quantities := range []map[string]string{resources.Requests, resources.Limits} {
			for name, value := range quantities {
				if name != "cpu" && name != "memory" {
					return input, fmt.Errorf("%w: supported PostgreSQL resources are cpu and memory", ErrInvalid)
				}
				quantity, err := resource.ParseQuantity(value)
				if err != nil || quantity.Sign() <= 0 {
					return input, fmt.Errorf("%w: PostgreSQL resource quantities must be positive", ErrInvalid)
				}
			}
		}
	}
	return input, nil
}

func postgresMajor(image string) int {
	match := regexp.MustCompile(`:[0-9]{2}\.[0-9]+(?:[.-][a-zA-Z0-9_.-]+)?(?:@sha256:[a-f0-9]{64})?$`).FindString(image)
	if match == "" {
		return 0
	}
	major, _ := strconv.Atoi(strings.Split(strings.TrimPrefix(match, ":"), ".")[0])
	if major < 14 || major > 18 {
		return 0
	}
	return major
}

// PutPostgres accepts a durable isolated PostgreSQL create or explicit update.
func (manager Manager) PutPostgres(ctx context.Context, organization, space, name string, input PostgresInput, apply bool) (Postgres, error) {
	if err := validateNames(organization, space, name); err != nil {
		return Postgres{}, err
	}
	action := "create"
	if apply {
		action = "apply"
	}
	return manager.acceptPostgres(ctx, organization, space, name, input, action, postgresTask{})
}

// ListPostgres returns accepted live managed databases in one existing space.
func (manager Manager) ListPostgres(ctx context.Context, organization, space string) ([]Postgres, error) {
	if _, err := manager.GetSpace(ctx, organization, space); err != nil {
		return nil, err
	}
	records, err := manager.database.List(ctx, "postgres")
	if err != nil {
		return nil, err
	}
	result := []Postgres{}
	for _, record := range records {
		var stored postgresRecord
		if err := json.Unmarshal(record.Value, &stored); err != nil {
			return nil, err
		}
		if stored.Organization == organization && stored.Space == space && stored.Postgres.State != "removed" {
			result = append(result, stored.Postgres)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

// GetPostgres returns safe accepted state and independently refreshed controller protection.
func (manager Manager) GetPostgres(ctx context.Context, organization, space, name string) (Postgres, error) {
	stored, _, err := loadDocument[postgresRecord](ctx, manager.database, "postgres", addonKey(organization, space, name))
	if err != nil {
		return Postgres{}, err
	}
	if stored.Postgres.State == "removed" {
		return Postgres{}, ErrNotFound
	}
	client, input, err := manager.postgresClient(ctx, stored, false)
	if err != nil {
		stored.Postgres.Protection.Backups, stored.Postgres.Protection.Wal = "unavailable", "unavailable"
		stored.Postgres.Protection.Message = "live PostgreSQL protection could not be observed"
		if stored.ActiveOperation == "" {
			stored.Postgres.State = "unavailable"
		}
		return stored.Postgres, nil
	}
	observed, err := client.Observe(ctx, input)
	if err == nil {
		stored.Postgres.Protection = postgresProtection(stored.Postgres.Protection, observed)
		if stored.ActiveOperation == "" {
			stored.Postgres.State = "degraded"
			if observed.Ready {
				stored.Postgres.State = "ready"
			}
		}
	} else {
		stored.Postgres.Protection.Backups, stored.Postgres.Protection.Wal = "unavailable", "unavailable"
		stored.Postgres.Protection.Message = "live PostgreSQL protection could not be observed"
		if stored.ActiveOperation == "" {
			stored.Postgres.State = "unavailable"
		}
	}
	return stored.Postgres, nil
}

// DeletePostgres durably removes owned runtime resources while preserving immutable S3 archives.
func (manager Manager) DeletePostgres(ctx context.Context, organization, space, name string) (Postgres, error) {
	return manager.acceptPostgres(ctx, organization, space, name, PostgresInput{}, "delete", postgresTask{})
}

// GetPostgresOperation reads durable progress without repeating an external action.
func (manager Manager) GetPostgresOperation(ctx context.Context, id string) (Operation, error) {
	operation, _, err := loadDocument[Operation](ctx, manager.database, "postgres-operation", id)
	return operation, err
}

// GetPostgresEnv is the explicit credential endpoint; ordinary state never contains passwords.
func (manager Manager) GetPostgresEnv(ctx context.Context, organization, space, name string) (map[string]string, error) {
	stored, _, err := loadDocument[postgresRecord](ctx, manager.database, "postgres", addonKey(organization, space, name))
	if err != nil {
		return nil, err
	}
	if stored.Postgres.State == "removed" {
		return nil, ErrNotFound
	}
	client, input, err := manager.postgresClient(ctx, stored, false)
	if err != nil {
		return nil, err
	}
	return client.Environment(ctx, input)
}

// BackupPostgres schedules a real full backup for internal automation and acceptance checks.
func (manager Manager) BackupPostgres(ctx context.Context, organization, space, name string) (Operation, error) {
	result, err := manager.acceptPostgres(ctx, organization, space, name, PostgresInput{}, "backup", postgresTask{})
	if err != nil {
		return Operation{}, err
	}
	return manager.GetPostgresOperation(ctx, result.Operation)
}

func (manager Manager) acceptPostgres(ctx context.Context, organization, space, name string, input PostgresInput, action string, task postgresTask) (Postgres, error) {
	manager.runtime.storageMutex.Lock()
	defer manager.runtime.storageMutex.Unlock()
	if err := validateNames(organization, space, name); err != nil {
		return Postgres{}, err
	}
	target, err := manager.GetSpace(ctx, organization, space)
	if err != nil {
		return Postgres{}, err
	}
	key := addonKey(organization, space, name)
	stored, record, err := loadDocument[postgresRecord](ctx, manager.database, "postgres", key)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Postgres{}, err
	}
	exists := err == nil && stored.Postgres.State != "removed"
	if action == "create" || action == "apply" || action == "restore" {
		if exists && action == "apply" {
			input = mergePostgresInput(stored.Postgres.Specification, input)
		}
		input, err = normalizePostgres(input)
		if err != nil {
			return Postgres{}, err
		}
	}
	if !exists && (action == "delete" || action == "backup") {
		return Postgres{}, ErrNotFound
	}
	if exists && (action == "create" || action == "restore") {
		return Postgres{}, fmt.Errorf("%w: PostgreSQL name already exists", ErrConflict)
	}
	if exists && action == "apply" {
		previous := stored.Postgres.Specification
		if previous.Database != input.Database || previous.Username != input.Username || postgresMajor(previous.Image) != postgresMajor(input.Image) || previous.StorageClass != input.StorageClass {
			return Postgres{}, fmt.Errorf("%w: database, username, major version and storage class are immutable; restore to a new name", ErrInvalid)
		}
		prior, _ := resource.ParseQuantity(previous.Storage)
		next, _ := resource.ParseQuantity(input.Storage)
		if next.Cmp(prior) < 0 {
			return Postgres{}, fmt.Errorf("%w: PostgreSQL storage cannot shrink", ErrInvalid)
		}
	}
	archive := postgresArchive{}
	newResource := !exists
	if newResource {
		safe, storage, err := manager.ResolvePostgresStorage(ctx, organization)
		if err != nil {
			return Postgres{}, err
		}
		storageClient, err := objectstore.New(storage)
		if err != nil {
			return Postgres{}, err
		}
		if err := storageClient.Validate(ctx); err != nil {
			return Postgres{}, fmt.Errorf("%w: managed PostgreSQL requires available S3 read/write/list/delete access", ErrInvalid)
		}
		id, err := newId()
		if err != nil {
			return Postgres{}, err
		}
		archiveID, err := newId()
		if err != nil {
			return Postgres{}, err
		}
		cluster, _, err := loadDocument[Cluster](ctx, manager.database, "cluster", organization+"/"+target.Cluster)
		if err != nil {
			return Postgres{}, err
		}
		client, err := newClusterClient(cluster)
		if err != nil {
			return Postgres{}, err
		}
		vault, mount, err := openEnvironmentVault(ctx, client)
		if err != nil {
			return Postgres{}, fmt.Errorf("%w: open managed PostgreSQL vault: %v", ErrComponentsNotReady, err)
		}
		password := make([]byte, 32)
		if _, err := rand.Read(password); err != nil {
			return Postgres{}, err
		}
		credentialPath := "managed-postgres/" + id
		entry, err := vault.WriteKv(ctx, mount, credentialPath, openbao.WriteKvInput{Values: map[string]string{"password": base64.RawURLEncoding.EncodeToString(password)}, PlanID: id, CheckVersion: 0})
		if err != nil {
			return Postgres{}, fmt.Errorf("%w: store managed PostgreSQL credential: %v", ErrComponentsNotReady, err)
		}
		stored = postgresRecord{Organization: organization, Space: space, Cluster: target.Cluster, ID: id, ArchiveID: archiveID, CredentialsRef: credentialPath + "#" + strconv.Itoa(entry.Version), Postgres: Postgres{ID: id, Name: name, Namespace: target.Namespace, ClusterName: "pg-" + id[:20], State: "pending", RestoreCheck: PostgresRestoreCheck{Status: "never"}}}
		archive = postgresArchive{ID: archiveID, Organization: organization, Space: space, Name: name, ResourceID: id, Endpoint: storage.Endpoint, Region: storage.Region, Bucket: storage.Bucket, Prefix: strings.Trim(storage.Prefix, "/") + "/" + archiveID, ServerName: "archive-" + archiveID, PathStyle: storage.PathStyle, CredentialsRef: safe.CredentialsRef, MajorVersion: postgresMajor(input.Image), Specification: input, CreatedAt: time.Now().UTC()}
		org, _, orgErr := loadDocument[Organization](ctx, manager.database, "organization", organization)
		if orgErr != nil {
			return Postgres{}, orgErr
		}
		archive.OrganizationID = org.ID
	}
	if action == "apply" || newResource {
		stored.Postgres.Specification = input
	}
	if action != "delete" {
		if _, _, err := manager.ResolvePostgresStorage(ctx, organization); err != nil {
			return Postgres{}, err
		}
		client, clusterInput, err := manager.postgresClient(ctx, stored, false)
		if err != nil {
			return Postgres{}, err
		}
		snapshotClass := ""
		if stored.Postgres.Specification.Snapshots != "disabled" {
			snapshotClass, err = client.SnapshotClass(ctx, clusterInput.StorageClass, stored.Postgres.Specification.SnapshotClass)
			if err != nil {
				return Postgres{}, fmt.Errorf("%w: inspect CSI snapshots: %v", ErrComponentsNotReady, err)
			}
		}
		if snapshotClass == "" && stored.Postgres.Specification.Snapshots == "required" {
			return Postgres{}, fmt.Errorf("%w: CSI snapshots are required but no matching CSI driver and deleting VolumeSnapshotClass are installed", ErrInvalid)
		}
		stored.Postgres.Protection.Snapshots = "unsupported"
		if stored.Postgres.Specification.Snapshots == "disabled" {
			stored.Postgres.Protection.Snapshots = "disabled"
		}
		if snapshotClass != "" {
			stored.Postgres.Protection.Snapshots = "supported"
			stored.Postgres.Specification.SnapshotClass = snapshotClass
		}
	}
	manager.runtime.mutex.Lock()
	defer manager.runtime.mutex.Unlock()
	if manager.runtime.closing {
		return Postgres{}, errors.New("application manager is stopping")
	}
	current, latest, loadErr := loadDocument[postgresRecord](ctx, manager.database, "postgres", key)
	if loadErr != nil && !errors.Is(loadErr, ErrNotFound) {
		return Postgres{}, loadErr
	}
	if latest.Revision != record.Revision {
		return Postgres{}, ErrConflict
	}
	if current.ActiveOperation != "" || current.ActiveCheck != "" {
		return Postgres{}, fmt.Errorf("%w: PostgreSQL has an active operation or recovery check", ErrConflict)
	}
	operationID, err := newId()
	if err != nil {
		return Postgres{}, err
	}
	now := time.Now().UTC()
	stored.ActiveOperation = operationID
	stored.Postgres.Operation = operationID
	stored.Postgres.UpdatedAt = now
	stored.Postgres.State = "pending"
	stored.Postgres.Message = "PostgreSQL " + action + " accepted"
	if action == "delete" {
		stored.Postgres.State = "deleting"
	}
	task.Key, task.Action, task.Deadline = key, action, now.Add(manager.runtime.rolloutTimeout)
	operation := Operation{ID: operationID, SpaceID: target.ID, Status: "pending", Phase: "postgres-" + action, Message: stored.Postgres.Message, StartedAt: now, UpdatedAt: now}
	writes := []sqlite.Write{}
	for _, item := range []struct {
		collection, key string
		value           any
		revision        int64
	}{{"postgres", key, stored, record.Revision}, {"postgres-task", operationID, task, 0}, {"postgres-operation", operationID, operation, 0}} {
		write, err := documentWrite(item.collection, item.key, item.value, item.revision)
		if err != nil {
			return Postgres{}, err
		}
		writes = append(writes, write)
	}
	if newResource {
		write, err := documentWrite("postgres-archive", archive.ID, archive, 0)
		if err != nil {
			return Postgres{}, err
		}
		writes = append(writes, write)
	}
	if err := manager.database.Commit(ctx, writes); err != nil {
		return Postgres{}, translateStoreError(err)
	}
	if manager.runtime.operationsResumed {
		manager.startPostgresOperation(operation)
	}
	return stored.Postgres, nil
}

func mergePostgresInput(previous, input PostgresInput) PostgresInput {
	if input.Instances != 0 {
		previous.Instances = input.Instances
	}
	if input.Storage != "" {
		previous.Storage = input.Storage
	}
	if input.StorageClass != "" {
		previous.StorageClass = input.StorageClass
	}
	if input.Image != "" {
		previous.Image = input.Image
	}
	if input.Database != "" {
		previous.Database = input.Database
	}
	if input.Username != "" {
		previous.Username = input.Username
	}
	if input.Resources.Requests != nil {
		previous.Resources.Requests = input.Resources.Requests
	}
	if input.Resources.Limits != nil {
		previous.Resources.Limits = input.Resources.Limits
	}
	if input.Backup.Schedule != "" {
		previous.Backup.Schedule = input.Backup.Schedule
	}
	if input.Backup.RecoveryWindowDays != 0 {
		previous.Backup.RecoveryWindowDays = input.Backup.RecoveryWindowDays
	}
	if input.Snapshots != "" {
		previous.Snapshots = input.Snapshots
	}
	if input.SnapshotClass != "" {
		previous.SnapshotClass = input.SnapshotClass
	}
	if input.SnapshotSchedule != "" {
		previous.SnapshotSchedule = input.SnapshotSchedule
	}
	if input.SnapshotRetentionHours != 0 {
		previous.SnapshotRetentionHours = input.SnapshotRetentionHours
	}
	if input.RestoreCheck.Enabled != nil {
		previous.RestoreCheck.Enabled = input.RestoreCheck.Enabled
	}
	if input.RestoreCheck.Schedule != "" {
		previous.RestoreCheck.Schedule = input.RestoreCheck.Schedule
	}
	if input.RestoreCheck.TimeoutSeconds != 0 {
		previous.RestoreCheck.TimeoutSeconds = input.RestoreCheck.TimeoutSeconds
	}
	if input.RestoreCheck.Resources.Requests != nil {
		previous.RestoreCheck.Resources.Requests = input.RestoreCheck.Resources.Requests
	}
	if input.RestoreCheck.Resources.Limits != nil {
		previous.RestoreCheck.Resources.Limits = input.RestoreCheck.Resources.Limits
	}
	return previous
}
