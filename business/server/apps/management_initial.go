package apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/stefanmcshane/borq/foundations/kubeapps"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	"github.com/stefanmcshane/borq/foundations/sqlite"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kubeclient "k8s.io/client-go/kubernetes"
)

// Before OpenBao exists, accepted inputs live in an owned Kubernetes Secret.
// Durable operations contain its reference and captured owners, never inline credentials.
func (manager Manager) acceptInitialManagementConfiguration(ctx context.Context, configuration ManagementConfiguration, requestHash string) (Operation, error) {
	id, err := newId()
	if err != nil {
		return Operation{}, err
	}
	encoded, err := json.Marshal(configuration)
	if err != nil {
		return Operation{}, err
	}
	owners := map[string]managementAuthentication{}
	writes := []sqlite.Write{}
	for _, declared := range configuration.Organizations {
		organization, _, err := loadDocument[Organization](ctx, manager.database, "organization", declared.Name)
		if errors.Is(err, ErrNotFound) {
			organization.ID, err = newId()
			if err != nil {
				return Operation{}, err
			}
			organization.Name, organization.ManagedAddons = declared.Name, true
			if declared.Oidc != nil || declared.ManagedPocketId != nil {
				organization.AuthenticationState = "queued"
			}
			write, err := documentWrite("organization", declared.Name, organization, 0)
			if err != nil {
				return Operation{}, err
			}
			writes = append(writes, write)
		} else if err != nil {
			return Operation{}, err
		}
		_, revision, err := loadDocument[organizationOidcRecord](ctx, manager.database, "organization-oidc", organization.ID)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return Operation{}, err
		}
		owners[declared.Name] = managementAuthentication{OrganizationID: organization.ID, Revision: revision.Revision}
	}
	client, namespace, err := manager.managementInputClient()
	if err != nil {
		return Operation{}, err
	}
	name := "borq-management-input-" + id
	immutable := true // Kubernetes owns this optional flag through a pointer.
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: managementInputLabels(id)}, Immutable: &immutable, Data: map[string][]byte{"configuration.json": encoded}}
	created, err := client.CoreV1().Secrets(namespace).Create(ctx, secret, metav1.CreateOptions{})
	if err != nil {
		return Operation{}, errors.New("could not stage private management configuration in Kubernetes")
	}
	accepted := false
	defer func() {
		if !accepted {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			_ = client.CoreV1().Secrets(namespace).Delete(cleanup, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &created.UID}})
		}
	}()
	now := time.Now().UTC()
	operation := Operation{ID: id, Status: "queued", Phase: "management", Message: "management configuration accepted", Resources: []kubeapps.ResourceStatus{}, StartedAt: now, UpdatedAt: now}
	task := managementTask{InputRef: name, InputUID: string(created.UID), RequestHash: requestHash, Authentication: owners, Hostnames: managementRouteHostnames(configuration), Deadline: now.Add(manager.runtime.rolloutTimeout * 4)}
	taskWrite, err := documentWrite("management-task", id, task, 0)
	if err != nil {
		return Operation{}, err
	}
	operationWrite, err := documentWrite("management-operation", id, operation, 0)
	if err != nil {
		return Operation{}, err
	}
	writes = append(writes, taskWrite, operationWrite)
	if err := manager.database.Commit(ctx, writes); err != nil {
		return Operation{}, err
	}
	accepted = true
	manager.startManagementOperation(operation)
	return operation, nil
}

func managementInputLabels(id string) map[string]string {
	return map[string]string{"app.kubernetes.io/managed-by": "borq", "app.kubernetes.io/part-of": "borq", "app.kubernetes.io/name": "borq-management-input", "borq.run/management-input": id}
}

func (manager Manager) managementInputClient() (*kubeclient.Clientset, string, error) {
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: manager.runtime.managementKubeconfigPath}, logger.Logger{})
	if err != nil {
		return nil, "", err
	}
	configuration := connection.RestConfig()
	client, err := kubeclient.NewForConfig(&configuration)
	return client, manager.networkDnsNamespace(), err
}

func (manager Manager) prepareInitialManagementConfiguration(ctx context.Context, operation Operation, task managementTask) (managementTask, error) {
	client, namespace, err := manager.managementInputClient()
	if err != nil {
		return task, err
	}
	secret, err := client.CoreV1().Secrets(namespace).Get(ctx, task.InputRef, metav1.GetOptions{})
	if err != nil || secret.Labels["app.kubernetes.io/managed-by"] != "borq" || secret.Labels["borq.run/management-input"] != operation.ID ||
		string(secret.UID) != task.InputUID || secret.Immutable == nil || !*secret.Immutable {
		return task, errors.New("private management input is unavailable or has changed ownership")
	}
	var configuration ManagementConfiguration
	if err := json.Unmarshal(secret.Data["configuration.json"], &configuration); err != nil {
		return task, errors.New("private management input is invalid")
	}
	digest, err := managementRequestDigest(configuration)
	if err != nil {
		return task, err
	}
	matches, err := task.matchesRequest(digest)
	if err != nil || !matches {
		return task, fmt.Errorf("%w: private management input changed after acceptance", ErrConflict)
	}
	if err := manager.validateInitialManagementOwners(ctx, configuration, task.Authentication); err != nil {
		return task, err
	}
	if err := manager.applyManagementNetworkConfiguration(ctx, configuration); err != nil {
		return task, err
	}
	if _, err := manager.ReconcileManagementComponents(ctx, false); err != nil {
		return task, err
	}
	manager.runtime.mutex.Lock()
	defer manager.runtime.mutex.Unlock()
	if err := manager.validateInitialManagementOwners(ctx, configuration, task.Authentication); err != nil {
		return task, err
	}
	safe, err := manager.stageManagementConfiguration(ctx, configuration)
	if err != nil {
		return task, err
	}
	accepted := false
	defer func() {
		if !accepted {
			manager.cleanManagementKubeconfigs(safe)
		}
	}()
	owners, identityWrites, err := manager.stageManagementAuthentication(ctx, &safe)
	if err != nil {
		return task, err
	}
	publications, environments, err := manager.stageManagementSecretGroups(ctx, &safe, owners)
	if err != nil {
		return task, err
	}
	safe.Network, err = manager.safeManagementNetwork(ctx)
	if err != nil {
		return task, err
	}
	task.Configuration, task.Authentication, task.SecretGroups, task.PocketIdEnvironments = safe, owners, publications, environments
	task.ClusterTailscaleBases = managementClusterTailscaleBases(safe)
	task.InputCleanupRef, task.InputCleanupUID = task.InputRef, task.InputUID
	task.InputRef, task.InputUID = "", ""
	_, previous, err := loadDocument[managementTask](ctx, manager.database, "management-task", operation.ID)
	if err != nil {
		return task, err
	}
	write, err := documentWrite("management-task", operation.ID, task, previous.Revision)
	if err != nil {
		return task, err
	}
	if err := manager.database.Commit(ctx, append(identityWrites, write)); err != nil {
		return task, err
	}
	accepted = true
	return task, nil
}

func (manager Manager) validateInitialManagementOwners(ctx context.Context, configuration ManagementConfiguration, accepted map[string]managementAuthentication) error {
	for name, owner := range accepted {
		_, record, err := manager.managementOidcOwner(ctx, name, owner)
		if err != nil {
			return err
		}
		if record.Revision != owner.Revision {
			return fmt.Errorf("%w: organization OIDC settings changed while management services were being configured", ErrConflict)
		}
		for _, organization := range configuration.Organizations {
			if organization.Name != name || organization.ManagedPocketId == nil {
				continue
			}
			pocket, _, err := manager.readPocketId(ctx, owner.OrganizationID)
			if errors.Is(err, ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			if err := matchManagementPocketId(pocket, *organization.ManagedPocketId, organization.oidcInput()); err != nil {
				return err
			}
		}
	}
	return nil
}

func (manager Manager) deleteManagementInput(ctx context.Context, task managementTask) error {
	name, uid := task.InputRef, task.InputUID
	if name == "" {
		name, uid = task.InputCleanupRef, task.InputCleanupUID
	}
	if name == "" {
		return nil
	}
	client, namespace, err := manager.managementInputClient()
	if err != nil {
		return err
	}
	identity := types.UID(uid)
	err = client.CoreV1().Secrets(namespace).Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &identity}})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}
