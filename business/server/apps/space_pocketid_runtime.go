package apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/business/server/sqliteworkload"
	"github.com/stefanmcshane/borq/foundations/kubeapps"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	"github.com/stefanmcshane/borq/foundations/pocketid"
	"github.com/stefanmcshane/borq/foundations/sqlite"
	"k8s.io/client-go/rest"
)

func (manager Manager) resumeSpacePocketIdOperations(ctx context.Context) error {
	records, err := manager.database.List(ctx, "pocketid-operation")
	if err != nil {
		return err
	}
	for _, record := range records {
		var operation Operation
		if err := json.Unmarshal(record.Value, &operation); err != nil {
			return err
		}
		if !operationFinished(operation.Status) {
			manager.startSpacePocketIdOperation(operation)
		}
	}
	return nil
}

func (manager Manager) startSpacePocketIdOperation(operation Operation) {
	manager.runtime.workers.Add(1)
	go func() { defer manager.runtime.workers.Done(); manager.runSpacePocketIdOperation(operation) }()
}

func (manager Manager) runSpacePocketIdOperation(operation Operation) {
	select {
	case manager.runtime.addonSlots <- struct{}{}:
		defer func() { <-manager.runtime.addonSlots }()
	case <-manager.runtime.context.Done():
		return
	}
	ctx, cancel := context.WithTimeout(manager.runtime.context, manager.runtime.rolloutTimeout)
	defer cancel()
	task, _, err := loadDocument[spacePocketIdTask](ctx, manager.database, "pocketid-task", operation.ID)
	if err != nil {
		return
	}
	stored, _, err := loadDocument[spacePocketIdRecord](ctx, manager.database, "space-pocketid", task.Key)
	if err != nil || stored.ActiveOperation != operation.ID {
		return
	}
	workErr := manager.reconcileSpacePocketId(ctx, stored, task.Remove)
	if manager.runtime.context.Err() != nil {
		return
	}
	for {
		if err := manager.finishSpacePocketId(operation, task, workErr); err == nil {
			return
		}
		if manager.waitForPoll(manager.runtime.context) != nil {
			return
		}
	}
}

func (manager Manager) spacePocketIdCredentials(ctx context.Context, stored spacePocketIdRecord) (map[string]string, error) {
	encoded, err := manager.readManagementSecret(ctx, "space-pocketid", stored.OrganizationID, stored.CredentialsRef, "credentials")
	if err != nil {
		return nil, errors.New("Pocket ID credentials are unavailable; restore their original vault entry before retrying")
	}
	var values map[string]string
	if json.Unmarshal([]byte(encoded), &values) != nil || len(values["encryptionKey"]) < 32 || len(values["apiKey"]) < 32 {
		return nil, errors.New("Pocket ID credentials are invalid; original keys will not be regenerated")
	}
	if err := (sqliteworkload.ArchiveKey{Identity: values["ageIdentity"], Recipient: values["ageRecipient"]}).Validate(); err != nil {
		return nil, err
	}
	return values, nil
}

func (manager Manager) spacePocketIdDeployment(ctx context.Context, stored spacePocketIdRecord) (platform.SpacePocketIdInput, error) {
	current, _, err := manager.readSpacePocketId(ctx, stored.Organization, stored.Space, stored.PocketId.Name)
	if err != nil || current.PocketId.ID != stored.PocketId.ID {
		return platform.SpacePocketIdInput{}, fmt.Errorf("%w: Pocket ID ownership changed", ErrConflict)
	}
	organization, _, err := loadDocument[Organization](ctx, manager.database, "organization", stored.Organization)
	if err != nil || organization.ID != stored.OrganizationID {
		return platform.SpacePocketIdInput{}, fmt.Errorf("%w: Pocket ID organization ownership changed", ErrConflict)
	}
	cluster, _, err := loadDocument[Cluster](ctx, manager.database, "cluster", stored.Organization+"/"+stored.Cluster)
	if err != nil {
		return platform.SpacePocketIdInput{}, err
	}
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: cluster.KubeconfigPath, Context: cluster.Context, InCluster: cluster.InCluster}, logger.Logger{})
	if err != nil {
		return platform.SpacePocketIdInput{}, err
	}
	credentials, err := manager.spacePocketIdCredentials(ctx, stored)
	if err != nil {
		return platform.SpacePocketIdInput{}, err
	}
	input := stored.PocketId.Specification
	return platform.SpacePocketIdInput{Connection: connection, Namespace: stored.PocketId.Namespace, ID: stored.PocketId.ID, IssuerURL: input.IssuerURL, CertificateIssuer: input.CertificateIssuer,
		ArchiveKey: sqliteworkload.ArchiveKey{Identity: credentials["ageIdentity"], Recipient: credentials["ageRecipient"]}, Storage: input.Storage, StorageClass: input.StorageClass, Image: input.Image, EncryptionKey: credentials["encryptionKey"], ApiKey: credentials["apiKey"], RequireExistingStorage: stored.Initialized, Timeout: manager.runtime.rolloutTimeout}, nil
}

func spacePocketIdClient(input platform.SpacePocketIdInput, endpoint platform.PocketIdEndpoint) (pocketid.Client, error) {
	configuration := input.Connection.RestConfig()
	// The Kubernetes transport requires a pointer to its configuration.
	client, err := rest.HTTPClientFor(&configuration)
	if err != nil {
		return pocketid.Client{}, err
	}
	return pocketid.New(pocketid.Config{BaseURL: strings.TrimRight(configuration.Host, "/") + "/api/v1/namespaces/" + endpoint.Namespace + "/services/http:" + endpoint.ServiceName + ":1411/proxy", ApiKey: input.ApiKey, HttpClient: client})
}

func spacePocketIdAdminID(id string) string {
	return id[:8] + "-" + id[8:12] + "-" + id[12:16] + "-" + id[16:20] + "-" + id[20:]
}

func (manager Manager) reconcileSpacePocketId(ctx context.Context, stored spacePocketIdRecord, remove bool) error {
	input, err := manager.spacePocketIdDeployment(ctx, stored)
	if err != nil {
		return err
	}
	if remove {
		return platform.RemoveSpacePocketId(ctx, input)
	}
	backup, backupCredentials, err := manager.spacePocketIdBackup(ctx, stored)
	if err != nil {
		return err
	}
	input.BackupDestination, input.BackupCredentials = backup, backupCredentials
	cluster, _, err := loadDocument[Cluster](ctx, manager.database, "cluster", stored.Organization+"/"+stored.Cluster)
	if err != nil {
		return err
	}
	namespaceClient, err := newClusterClient(cluster)
	if err != nil {
		return err
	}
	// An addon may be the first workload in its Space.
	if err := namespaceClient.EnsureNamespace(ctx, kubeapps.NamespaceInput{Name: stored.PocketId.Namespace, TargetID: stored.SpaceID}); err != nil {
		return err
	}
	endpoint, err := platform.InstallSpacePocketId(ctx, input)
	if err != nil {
		return errors.New("Pocket ID could not start; check its volume, credentials, image and resource ownership")
	}
	client, err := spacePocketIdClient(input, endpoint)
	if err != nil {
		return errors.New("Pocket ID's private service is unavailable")
	}
	if stored.Initialized {
		if err := client.VerifyAdmin(ctx, spacePocketIdAdminID(stored.PocketId.ID)); err != nil {
			return errors.New("The original Pocket ID administrator is missing or disabled; restore its data before retrying")
		}
	} else {
		specification := stored.PocketId.Specification
		if err := client.EnsureAdmin(ctx, pocketid.Admin{ID: spacePocketIdAdminID(stored.PocketId.ID), Username: specification.AdminUsername, Email: specification.AdminEmail}); err != nil {
			return errors.New("Pocket ID administrator setup failed; public setup remains blocked")
		}
		// Persist before publishing. Recovery must verify the existing identity, never bootstrap an empty database.
		manager.runtime.mutex.Lock()
		current, record, err := loadDocument[spacePocketIdRecord](ctx, manager.database, "space-pocketid", addonKey(stored.Organization, stored.Space, stored.PocketId.Name))
		if err == nil && current.ActiveOperation == stored.ActiveOperation && current.PocketId.ID == stored.PocketId.ID {
			current.Initialized = true
			var write sqlite.Write
			write, err = documentWrite("space-pocketid", addonKey(stored.Organization, stored.Space, stored.PocketId.Name), current, record.Revision)
			if err == nil {
				err = manager.database.Commit(ctx, []sqlite.Write{write})
			}
		} else if err == nil {
			err = ErrConflict
		}
		manager.runtime.mutex.Unlock()
		if err != nil {
			return errors.New("Pocket ID initialization could not be saved; public access remains blocked")
		}
	}
	if err := platform.PublishSpacePocketId(ctx, input); err != nil {
		return errors.New("Pocket ID could not publish HTTPS; check its hostname, certificate issuer and route ownership")
	}
	return nil
}

func (manager Manager) finishSpacePocketId(operation Operation, task spacePocketIdTask, workErr error) error {
	manager.runtime.mutex.Lock()
	defer manager.runtime.mutex.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stored, record, err := loadDocument[spacePocketIdRecord](ctx, manager.database, "space-pocketid", task.Key)
	if err != nil {
		return err
	}
	if stored.ActiveOperation != operation.ID {
		return nil
	}
	_, opRecord, err := loadDocument[Operation](ctx, manager.database, "pocketid-operation", operation.ID)
	if err != nil {
		return err
	}
	stored.ActiveOperation = ""
	stored.PocketId.State, stored.PocketId.Message = "ready", "Pocket ID is ready. Enroll the administrator's passkey to finish setup."
	operation.Status = "succeeded"
	if task.Remove {
		stored.PocketId.State, stored.PocketId.Message = "removed", "Pocket ID serving resources removed; SQLite data and encryption keys retained. Apply the same name to resume."
	}
	if workErr != nil {
		operation.Status, stored.PocketId.State, stored.PocketId.Message = "failed", "failed", workErr.Error()
	}
	operation.Message, operation.UpdatedAt = stored.PocketId.Message, time.Now().UTC()
	stored.PocketId.UpdatedAt = operation.UpdatedAt
	write, err := documentWrite("space-pocketid", task.Key, stored, record.Revision)
	if err != nil {
		return err
	}
	opWrite, err := documentWrite("pocketid-operation", operation.ID, operation, opRecord.Revision)
	if err != nil {
		return err
	}
	return manager.database.Commit(ctx, []sqlite.Write{write, opWrite})
}

// NewPocketIdEnrollment creates an ephemeral initial administrator link on explicit request.
func (manager Manager) NewPocketIdEnrollment(ctx context.Context, organization, space, name string) (string, error) {
	stored, _, err := manager.readSpacePocketId(ctx, organization, space, name)
	if err != nil {
		return "", err
	}
	if stored.PocketId.State != "ready" || !stored.Initialized || stored.ActiveOperation != "" {
		return "", fmt.Errorf("%w: Pocket ID must be ready before enrollment", ErrConflict)
	}
	input, err := manager.spacePocketIdDeployment(ctx, stored)
	if err != nil {
		return "", err
	}
	client, err := spacePocketIdClient(input, platform.PocketIdEndpoint{Namespace: input.Namespace, ServiceName: "pocketid-" + input.ID, Port: 1411})
	if err != nil {
		return "", err
	}
	token, err := client.NewOneTimeAccess(ctx, spacePocketIdAdminID(input.ID))
	if errors.Is(err, pocketid.ErrAlreadyEnrolled) {
		return "", errors.Join(ErrConflict, ErrPocketIdAlreadyEnrolled)
	}
	if err != nil {
		return "", err
	}
	return input.IssuerURL + "/lc/" + url.PathEscape(token), nil
}
