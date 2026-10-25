// Package postgres reconciles CloudNativePG resources and observes real SQL and controller results.
package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	kube "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

// Client uses the registered target credentials for every Kubernetes and SQL request.
type Client struct {
	dynamic dynamic.Interface
	kube    kube.Interface
	config  rest.Config
}

// New constructs a concrete CNPG client for one registered Kubernetes target.
func New(config kubernetes.Config) (Client, error) {
	connection, err := kubernetes.NewConnection(config, logger.Logger{})
	if err != nil {
		return Client{}, err
	}
	restConfig := connection.RestConfig()
	dynamicClient, err := dynamic.NewForConfig(&restConfig)
	if err != nil {
		return Client{}, err
	}
	kubeClient, err := kube.NewForConfig(&restConfig)
	if err != nil {
		return Client{}, err
	}
	return Client{dynamic: dynamicClient, kube: kubeClient, config: restConfig}, nil
}

// Storage freezes one physical archive destination and transient credential material.
type Storage struct {
	// Endpoint, Region, Bucket, Prefix, ServerName identify the immutable physical location.
	Endpoint, Region, Bucket, Prefix, ServerName string
	// AccessKey, SecretKey, SessionToken are projected into a Kubernetes Secret only.
	AccessKey, SecretKey, SessionToken string
	// PathStyle configures S3-compatible addressing.
	PathStyle bool
}

// Recovery identifies one validated source backup.
type Recovery struct {
	// Storage is the original read-only archive source.
	Storage Storage
	// BackupID is the exact Barman physical backup identifier.
	BackupID string
	// SnapshotName selects an actual ready CSI PGDATA snapshot for verification.
	SnapshotName string
	// Target optionally selects a previously validated point in time.
	Target time.Time
}

// Input is a normalized cluster policy and immutable ownership identity.
type Input struct {
	// Namespace, Name, Owner isolate all reconciled resources.
	Namespace, Name, Owner string
	// Organization, SpaceID label collected telemetry.
	Organization, SpaceID string
	// Instances is the desired instance count.
	Instances int
	// StorageSize, StorageClass, Image, Database, Username configure the database.
	StorageSize, StorageClass, Image, Database, Username string
	// EnvironmentExport publishes application connection values through External Secrets.
	EnvironmentExport EnvironmentExport
	// BootstrapPassword is supplied only until CNPG has initialized its application role.
	BootstrapPassword string `json:"-"`
	// Credentials identifies the OpenBao-owned rotating application credential.
	Credentials CredentialSource
	// Requests and Limits constrain each instance.
	Requests, Limits map[string]string
	// Storage identifies the destination for full backups and WAL.
	Storage Storage
	// BackupEnabled derives outgoing backup and verification activity from the accepted policy.
	BackupEnabled bool
	// BackupSchedule uses six cron fields; RetentionDays is the recovery window.
	BackupSchedule string
	RetentionDays  int
	// SnapshotClass is empty when snapshots are unsupported or disabled.
	SnapshotClass, SnapshotSchedule string
	// Recovery is present only while bootstrapping an isolated restore.
	Recovery *Recovery
	// Isolated suppresses outgoing archives and backup schedules for temporary verification copies.
	Isolated bool
	// Check configures the Kubernetes trigger for server-owned recovery verification.
	Check Check
}

// EnvironmentExport identifies an immutable environment group and its OpenBao path.
type EnvironmentExport struct {
	// Name identifies the environment group in the owning space.
	Name string
	// VaultPath is the KV v2 key below the target store mount.
	VaultPath string
}

// Check describes a scoped HTTP trigger, never an administrator credential.
type Check struct {
	// Enabled schedules automatic isolated recovery checks.
	Enabled bool
	// Schedule uses Kubernetes five-field cron syntax.
	Schedule string
	// ApiURL is the resource-scoped restore-check endpoint.
	ApiURL string
	// TimeoutSeconds bounds the trigger Job; the durable server workflow has its own deadline.
	TimeoutSeconds int
}

func resource(kind string) schema.GroupVersionResource {
	group, version := "postgresql.cnpg.io", "v1"
	plural := map[string]string{"Cluster": "clusters", "Backup": "backups", "ScheduledBackup": "scheduledbackups", "ObjectStore": "objectstores", "CronJob": "cronjobs", "ServiceAccount": "serviceaccounts", "ConfigMap": "configmaps", "VolumeSnapshot": "volumesnapshots", "VolumeSnapshotClass": "volumesnapshotclasses", "PushSecret": "pushsecrets", "ExternalSecret": "externalsecrets", "VaultDynamicSecret": "vaultdynamicsecrets"}[kind]
	switch kind {
	case "PushSecret":
		group, version = "external-secrets.io", "v1alpha1"
	case "ExternalSecret":
		group, version = "external-secrets.io", "v1"
	case "VaultDynamicSecret":
		group, version = "generators.external-secrets.io", "v1alpha1"
	case "ObjectStore":
		group = "barmancloud.cnpg.io"
	case "CronJob":
		group = "batch"
	case "ServiceAccount", "ConfigMap":
		group = ""
	case "VolumeSnapshot", "VolumeSnapshotClass":
		group = "snapshot.storage.k8s.io"
	}
	return schema.GroupVersionResource{Group: group, Version: version, Resource: plural}
}

func object(apiVersion, kind, name, namespace, owner string, spec map[string]any) map[string]any {
	result := map[string]any{"apiVersion": apiVersion, "kind": kind, "metadata": map[string]any{"name": name, "namespace": namespace, "labels": map[string]any{"borq.run/postgres-id": owner}}}
	if spec != nil {
		result["spec"] = spec
	}
	return result
}

func plugin(store, server string) map[string]any {
	return map[string]any{"name": "barman-cloud.cloudnative-pg.io", "parameters": map[string]any{"barmanObjectName": store, "serverName": server}}
}

func objectStore(input Input, name string, storage Storage) map[string]any {
	secret := input.Name + "-s3"
	credentials := map[string]any{"accessKeyId": map[string]any{"name": secret, "key": "ACCESS_KEY_ID"}, "secretAccessKey": map[string]any{"name": secret, "key": "ACCESS_SECRET_KEY"}, "region": map[string]any{"name": secret, "key": "REGION"}}
	credentials["sessionToken"] = map[string]any{"name": secret, "key": "SESSION_TOKEN"}
	configuration := map[string]any{"destinationPath": "s3://" + storage.Bucket + "/" + strings.Trim(storage.Prefix, "/"), "s3Credentials": credentials, "wal": map[string]any{"compression": "gzip"}, "data": map[string]any{"compression": "gzip"}}
	if storage.Endpoint != "" {
		configuration["endpointURL"] = storage.Endpoint
	}
	return object("barmancloud.cnpg.io/v1", "ObjectStore", name, input.Namespace, input.Owner, map[string]any{"configuration": configuration, "retentionPolicy": strconv.Itoa(input.RetentionDays) + "d", "instanceSidecarConfiguration": map[string]any{"env": []any{map[string]any{"name": "AWS_CONFIG_FILE", "value": "/projected/aws-config"}}}})
}

// Manifests renders exact CNPG/plugin resources without persisting secret contents.
func Manifests(input Input) []map[string]any {
	store := input.Name + "-store"
	walPlugin := plugin(store, input.Storage.ServerName)
	walPlugin["isWALArchiver"] = true
	storage := map[string]any{"size": input.StorageSize}
	if input.StorageClass != "" {
		storage["storageClass"] = input.StorageClass
	}
	spec := map[string]any{"env": []any{map[string]any{"name": "BORQ_BACKUP_ARCHIVE", "value": backupPolicyMarker(input)}}, "instances": int64(input.Instances), "imageName": input.Image, "storage": storage, "enableSuperuserAccess": false, "postgresql": map[string]any{"parameters": map[string]any{"archive_timeout": "60s"}}, "plugins": []any{walPlugin}, "inheritedMetadata": map[string]any{"labels": map[string]any{"borq.run/postgres-id": input.Owner, "borq.run/organization": input.Organization, "borq.run/space-id": input.SpaceID, "borq.run/addon-name": input.Name, "borq.run/logs": "true", "borq.run/metrics": "true"}}, "bootstrap": map[string]any{"initdb": map[string]any{"database": input.Database, "owner": input.Username, "secret": map[string]any{"name": input.Name + "-app"}}}}
	if input.Credentials.Host != "" {
		spec["certificates"] = map[string]any{"serverAltDNSNames": []any{input.Credentials.Host}}
	}
	if len(input.Requests) > 0 || len(input.Limits) > 0 {
		spec["resources"] = map[string]any{"requests": input.Requests, "limits": input.Limits}
	}
	style := "virtual"
	if input.Storage.PathStyle {
		style = "path"
	}
	config := object("v1", "ConfigMap", input.Name+"-aws", input.Namespace, input.Owner, nil)
	config["data"] = map[string]any{"config": "[default]\ns3 =\n    addressing_style = " + style + "\n"}
	spec["projectedVolumeTemplate"] = map[string]any{"sources": []any{map[string]any{"configMap": map[string]any{"name": input.Name + "-aws", "items": []any{map[string]any{"key": "config", "path": "aws-config"}}}}}}
	manifests := []map[string]any{config}
	if input.BackupEnabled && !input.Isolated {
		manifests = append(manifests, objectStore(input, store, input.Storage))
	}
	if input.Recovery != nil {
		sourceStore := input.Name + "-source"
		manifests = append(manifests, objectStore(input, sourceStore, input.Recovery.Storage))
		target := map[string]any{"backupID": input.Recovery.BackupID}
		if !input.Recovery.Target.IsZero() {
			target["targetTime"] = input.Recovery.Target.UTC().Format(time.RFC3339Nano)
		} else {
			target["targetImmediate"] = true
		}
		spec["bootstrap"] = map[string]any{"recovery": map[string]any{"source": "source", "database": input.Database, "owner": input.Username, "secret": map[string]any{"name": input.Name + "-app"}, "recoveryTarget": target}}
		spec["externalClusters"] = []any{map[string]any{"name": "source", "plugin": plugin(sourceStore, input.Recovery.Storage.ServerName)}}
		if input.Recovery.SnapshotName != "" {
			recovery := spec["bootstrap"].(map[string]any)["recovery"].(map[string]any)
			recovery["volumeSnapshots"] = map[string]any{"storage": map[string]any{"apiGroup": "snapshot.storage.k8s.io", "kind": "VolumeSnapshot", "name": input.Recovery.SnapshotName}}
			recovery["recoveryTarget"] = map[string]any{"targetImmediate": true}
		}
	}
	if input.Isolated || !input.BackupEnabled {
		spec["plugins"] = []any{}
		spec["backup"] = map[string]any{}
	}
	if input.BackupEnabled && !input.Isolated && input.SnapshotClass != "" {
		spec["backup"] = map[string]any{"volumeSnapshot": map[string]any{"className": input.SnapshotClass, "snapshotOwnerReference": "backup", "online": true, "onlineConfiguration": map[string]any{"waitForArchive": true, "immediateCheckpoint": true}}}
	}
	manifests = append(manifests, object("postgresql.cnpg.io/v1", "Cluster", input.Name, input.Namespace, input.Owner, spec))
	backup := map[string]any{"schedule": input.BackupSchedule, "suspend": true, "immediate": false, "backupOwnerReference": "self", "cluster": map[string]any{"name": input.Name}, "method": "plugin", "pluginConfiguration": map[string]any{"name": "barman-cloud.cloudnative-pg.io"}}
	if input.BackupEnabled && !input.Isolated {
		manifests = append(manifests, object("postgresql.cnpg.io/v1", "ScheduledBackup", backupScheduleName(input), input.Namespace, input.Owner, backup))
	}
	if input.BackupEnabled && !input.Isolated && input.SnapshotClass != "" {
		manifests = append(manifests, snapshotSchedule(input, true))
	}
	if input.BackupEnabled && !input.Isolated && input.Check.Enabled && input.Check.ApiURL != "" {
		manifests = append(manifests, checkManifests(input)...)
	}
	return manifests
}

func snapshotSchedule(input Input, suspended bool) map[string]any {
	return object("postgresql.cnpg.io/v1", "ScheduledBackup", input.Name+"-snapshots", input.Namespace, input.Owner, map[string]any{"schedule": input.SnapshotSchedule, "suspend": suspended, "immediate": false, "backupOwnerReference": "self", "cluster": map[string]any{"name": input.Name}, "method": "volumeSnapshot"})
}

func checkManifests(input Input) []map[string]any {
	name := input.Name + "-check"
	sa := object("v1", "ServiceAccount", name, input.Namespace, input.Owner, nil)
	sa["automountServiceAccountToken"] = false
	container := map[string]any{"name": "trigger", "image": "curlimages/curl:8.14.1", "command": []any{"/bin/sh", "-ec"}, "args": []any{`post=$(curl --silent --show-error --fail --retry 3 --max-time 60 -X POST -H "Authorization: Bearer $(cat /var/run/borq/token)" -H 'Content-Type: application/json' --data "{\"jobUid\":\"$JOB_UID\"}" "$BORQ_CHECK_URL")
operation=$(printf '%s' "$post" | sed -n 's/.*"id"[[:space:]]*:[[:space:]]*"\([0-9a-f]\{32\}\)".*/\1/p')
test "${#operation}" -eq 32
while :; do
  result=$(curl --silent --show-error --fail --retry 3 --max-time 60 -H "Authorization: Bearer $(cat /var/run/borq/token)" "$BORQ_CHECK_URL/$operation?jobUid=$JOB_UID")
  case "$result" in
    *'"status":"succeeded"'*|*'"status": "succeeded"'*) exit 0 ;;
    *'"status":"failed"'*|*'"status": "failed"'*) printf '%s\n' "$result"; exit 1 ;;
  esac
  sleep 5
done`}, "env": []any{map[string]any{"name": "BORQ_CHECK_URL", "value": input.Check.ApiURL}, map[string]any{"name": "JOB_UID", "valueFrom": map[string]any{"fieldRef": map[string]any{"fieldPath": "metadata.labels['batch.kubernetes.io/controller-uid']"}}}}, "volumeMounts": []any{map[string]any{"name": "token", "mountPath": "/var/run/borq", "readOnly": true}}, "resources": map[string]any{"requests": map[string]any{"cpu": "10m", "memory": "16Mi"}, "limits": map[string]any{"cpu": "100m", "memory": "64Mi"}}, "securityContext": map[string]any{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": map[string]any{"drop": []any{"ALL"}}}}
	pod := map[string]any{"serviceAccountName": name, "automountServiceAccountToken": false, "restartPolicy": "Never", "containers": []any{container}, "volumes": []any{map[string]any{"name": "token", "projected": map[string]any{"sources": []any{map[string]any{"serviceAccountToken": map[string]any{"path": "token", "audience": "borq-postgres-check", "expirationSeconds": int64(3600)}}}}}}}
	cron := object("batch/v1", "CronJob", name, input.Namespace, input.Owner, map[string]any{"schedule": input.Check.Schedule, "suspend": true, "timeZone": "Etc/UTC", "concurrencyPolicy": "Forbid", "successfulJobsHistoryLimit": int64(1), "failedJobsHistoryLimit": int64(2), "jobTemplate": map[string]any{"spec": map[string]any{"backoffLimit": int64(1), "activeDeadlineSeconds": int64(input.Check.TimeoutSeconds + 660), "ttlSecondsAfterFinished": int64(86400), "template": map[string]any{"metadata": map[string]any{"labels": map[string]any{"borq.run/postgres-id": input.Owner}}, "spec": pod}}}})
	return []map[string]any{sa, cron}
}

func (client Client) apply(ctx context.Context, manifest map[string]any, owner string) error {
	item := unstructured.Unstructured{Object: manifest}
	handle := client.dynamic.Resource(resource(item.GetKind())).Namespace(item.GetNamespace())
	current, err := handle.Get(ctx, item.GetName(), metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if err == nil && current.GetLabels()["borq.run/postgres-id"] != owner {
		return errors.New("managed PostgreSQL resource has conflicting ownership")
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	options := metav1.PatchOptions{FieldManager: "borq-postgres"}
	if item.GetKind() == "ScheduledBackup" || item.GetKind() == "CronJob" {
		// Kubernetes tracks imperative suspension separately from apply, even for the same manager.
		force := true
		options.Force = &force
	}
	_, err = handle.Patch(ctx, item.GetName(), types.ApplyPatchType, encoded, options)
	return err
}

func (client Client) secret(ctx context.Context, input Input, name string, values map[string]string) error {
	handle := client.kube.CoreV1().Secrets(input.Namespace)
	existing, err := handle.Get(ctx, name, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	desired := corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: input.Namespace, Labels: map[string]string{"borq.run/postgres-id": input.Owner, "cnpg.io/reload": "true"}}, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{}}
	for key, value := range values {
		desired.Data[key] = []byte(value)
	}
	if strings.HasSuffix(name, "-app") {
		desired.Type = corev1.SecretTypeBasicAuth
	}
	if apierrors.IsNotFound(err) {
		_, err = handle.Create(ctx, &desired, metav1.CreateOptions{})
		return err
	}
	if existing.Labels["borq.run/postgres-id"] != input.Owner {
		return errors.New("managed PostgreSQL Secret has conflicting ownership")
	}
	desired.ResourceVersion = existing.ResourceVersion
	_, err = handle.Update(ctx, &desired, metav1.UpdateOptions{})
	return err
}

// Reconcile applies idempotent cluster, backup, and scoped verification resources.
func (client Client) Reconcile(ctx context.Context, input Input) error {
	if _, err := client.kube.CoreV1().Namespaces().Get(ctx, input.Namespace, metav1.GetOptions{}); apierrors.IsNotFound(err) {
		_, err = client.kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: input.Namespace}}, metav1.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	} else if err != nil {
		return err
	}
	changingArchive, err := client.archiveSwitchRequired(ctx, input)
	if err != nil {
		return err
	}
	suspended := input
	if changingArchive {
		suspended.BackupEnabled = false
	}
	if err := client.suspendBackupSchedules(ctx, suspended); err != nil {
		return err
	}
	if !input.Isolated && (!input.BackupEnabled || changingArchive) {
		for {
			settled, err := client.backupWorkSettled(ctx, input)
			if err != nil {
				return err
			}
			if settled {
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
		}
	}
	if err := client.RefreshStorage(ctx, input); err != nil {
		return err
	}
	if input.BootstrapPassword != "" {
		if err := client.secret(ctx, input, input.Name+"-app", map[string]string{"username": input.Username, "password": input.BootstrapPassword}); err != nil {
			return err
		}
	}
	if err := client.ReconcileEnvironment(ctx, input); err != nil {
		return err
	}
	for _, manifest := range Manifests(input) {
		if err := client.apply(ctx, manifest, input.Owner); err != nil {
			return fmt.Errorf("reconcile %s: %w", manifest["kind"], err)
		}
	}

	return nil
}

// RefreshStorage updates only the credential projection and leaves the running database untouched.
func (client Client) RefreshStorage(ctx context.Context, input Input) error {
	return client.secret(ctx, input, input.Name+"-s3", map[string]string{"ACCESS_KEY_ID": input.Storage.AccessKey, "ACCESS_SECRET_KEY": input.Storage.SecretKey, "SESSION_TOKEN": input.Storage.SessionToken, "REGION": input.Storage.Region})
}

// SnapshotClass checks discovery, the selected storage provisioner, and a matching installed CSI snapshot class.
func (client Client) SnapshotClass(ctx context.Context, storageClass, snapshotClass string) (string, error) {
	classes, err := client.kube.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", err
	}
	provisioner := ""
	for _, class := range classes.Items {
		if class.Name == storageClass || storageClass == "" && (class.Annotations["storageclass.kubernetes.io/is-default-class"] == "true" || class.Annotations["storageclass.beta.kubernetes.io/is-default-class"] == "true") {
			provisioner = class.Provisioner
		}
	}
	if provisioner == "" {
		return "", nil
	}
	drivers, err := client.kube.StorageV1().CSIDrivers().List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", err
	}
	installed := false
	for _, driver := range drivers.Items {
		if driver.Name == provisioner {
			installed = true
			break
		}
	}
	if !installed {
		return "", nil
	}
	snapshots, err := client.dynamic.Resource(resource("VolumeSnapshotClass")).List(ctx, metav1.ListOptions{})
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	matches := []string{}
	for _, class := range snapshots.Items {
		driver, _, _ := unstructured.NestedString(class.Object, "driver")
		policy, _, _ := unstructured.NestedString(class.Object, "deletionPolicy")
		if driver == provisioner && policy == "Delete" && (snapshotClass == "" || class.GetName() == snapshotClass) {
			matches = append(matches, class.GetName())
		}
	}
	sort.Strings(matches)
	if len(matches) == 0 {
		return "", nil
	}
	return matches[0], nil
}

// Observation reports independently observed database and protection state.
type Observation struct {
	// Ready is true only after controller readiness and a real SQL query succeed.
	Ready bool
	// PolicyReady means the accepted outgoing backup policy has reached the current primary.
	PolicyReady bool
	// Wal and Backups report controller conditions and completed Backup resources.
	Wal, Backups string
	// Message summarizes observed health.
	Message string
	// CheckUnavailable records missing mandatory restore-test routing.
	CheckUnavailable bool
	// LastBackupAt records the latest complete backup resource timestamp.
	LastBackupAt time.Time
}

// Observe reads actual controller state and executes SQL against the current primary.
func (client Client) Observe(ctx context.Context, input Input) (Observation, error) {
	result := Observation{Wal: "pending", Backups: "pending", CheckUnavailable: input.BackupEnabled && !input.Isolated && (!input.Check.Enabled || input.Check.ApiURL == "")}
	cluster, err := client.dynamic.Resource(resource("Cluster")).Namespace(input.Namespace).Get(ctx, input.Name, metav1.GetOptions{})
	if err != nil {
		return result, err
	}
	conditions, _, _ := unstructured.NestedSlice(cluster.Object, "status", "conditions")
	ready := false
	for _, raw := range conditions {
		condition, _ := raw.(map[string]any)
		switch condition["type"] {
		case "Ready":
			ready = condition["status"] == "True"
		case "ContinuousArchiving":
			result.Wal = "failed"
			if condition["status"] == "True" {
				result.Wal = "ready"
			}
		}
	}
	backups, err := client.dynamic.Resource(resource("Backup")).Namespace(input.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return result, err
	}
	result.Backups, result.LastBackupAt = backupHealth(input, backups.Items, time.Now().UTC())
	primary, _, _ := unstructured.NestedString(cluster.Object, "status", "currentPrimary")
	if primary != "" {
		pod, err := client.kube.CoreV1().Pods(input.Namespace).Get(ctx, primary, metav1.GetOptions{})
		if err != nil {
			return result, err
		}
		result.PolicyReady = primaryPolicyReady(*cluster, *pod, input)
	}
	if !result.PolicyReady {
		result.Wal = "pending"
	}

	if !ready {
		result.Message = "waiting for PostgreSQL controller readiness"
		return result, nil
	}
	value, err := client.Query(ctx, input.Namespace, input.Name, input.Database, "SELECT 1")
	if err != nil {
		return result, err
	}
	result.Ready = strings.TrimSpace(value) == "1"
	result.Message = "PostgreSQL accepts SQL"
	return result, nil
}

// RequestBackup creates one idempotent full backup request with an operation-derived identity.
func (client Client) RequestBackup(ctx context.Context, input Input, operation string) error {
	manifest := object("postgresql.cnpg.io/v1", "Backup", input.Name+"-b-"+operation[:12], input.Namespace, input.Owner, map[string]any{"cluster": map[string]any{"name": input.Name}, "method": "plugin", "pluginConfiguration": map[string]any{"name": "barman-cloud.cloudnative-pg.io"}})
	manifest["metadata"].(map[string]any)["labels"].(map[string]any)["borq.run/postgres-archive"] = input.Storage.ServerName
	return client.apply(ctx, manifest, input.Owner)
}

// BackupComplete reads completion for precisely the requested full backup.
func (client Client) BackupComplete(ctx context.Context, input Input, operation string) (bool, error) {
	backup, err := client.dynamic.Resource(resource("Backup")).Namespace(input.Namespace).Get(ctx, input.Name+"-b-"+operation[:12], metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	if !backupInArchive(*backup, input) {
		return false, errors.New("backup belongs to a different archive")
	}
	phase, _, _ := unstructured.NestedString(backup.Object, "status", "phase")
	if phase == "failed" {
		return false, errors.New("PostgreSQL full backup failed; inspect controller logs")
	}
	return phase == "completed", nil
}

func (client Client) primary(ctx context.Context, namespace, name string) (string, error) {
	cluster, err := client.dynamic.Resource(resource("Cluster")).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	primary, _, _ := unstructured.NestedString(cluster.Object, "status", "currentPrimary")
	if primary == "" {
		return "", errors.New("PostgreSQL primary is not available")
	}
	return primary, nil
}

// Query executes a bounded noninteractive SQL command inside the current primary.
func (client Client) Query(ctx context.Context, namespace, name, database, query string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	primary, err := client.primary(ctx, namespace, name)
	if err != nil {
		return "", err
	}
	return client.exec(ctx, namespace, primary, []string{"env", "PGOPTIONS=-c statement_timeout=10000", "psql", "-X", "-A", "-t", "-v", "ON_ERROR_STOP=1", "--dbname", database, "--command", query})
}

func (client Client) exec(ctx context.Context, namespace, pod string, command []string) (string, error) {
	return client.execInput(ctx, namespace, pod, command, nil)
}

func (client Client) execInput(ctx context.Context, namespace, pod string, command []string, input io.Reader) (string, error) {
	request := client.kube.CoreV1().RESTClient().Post().Resource("pods").Name(pod).Namespace(namespace).SubResource("exec").VersionedParams(&corev1.PodExecOptions{Container: "postgres", Command: command, Stdin: input != nil, Stdout: true, Stderr: true}, scheme.ParameterCodec)
	executor, err := remotecommand.NewSPDYExecutor(&client.config, "POST", request.URL())
	if err != nil {
		return "", err
	}
	var output, diagnostic boundedBuffer
	if err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{Stdin: input, Stdout: &output, Stderr: &diagnostic}); err != nil {
		return "", errors.New("PostgreSQL command failed; inspect database readiness and controller logs")
	}
	return output.String(), nil
}

type boundedBuffer struct{ bytes.Buffer }

func (buffer *boundedBuffer) Write(value []byte) (int, error) {
	if buffer.Len()+len(value) > 1024*1024 {
		return 0, errors.New("PostgreSQL command output exceeded limit")
	}
	return buffer.Buffer.Write(value)
}

// Stats is a current SQL and volume observation with explicit disk availability.
type Stats struct {
	// DatabaseBytes, Connections, ActiveQueries, Transactions are live PostgreSQL statistics.
	DatabaseBytes, Connections, ActiveQueries, Transactions int64
	// DiskAvailable distinguishes an observed empty disk from unavailable volume statistics.
	DiskAvailable bool
	// DiskUsedBytes, DiskCapacityBytes, DiskAvailableBytes are measured by the primary filesystem.
	DiskUsedBytes, DiskCapacityBytes, DiskAvailableBytes int64
}

// Statistics executes bounded aggregate SQL and observes the actual PGDATA filesystem.
func (client Client) Statistics(ctx context.Context, input Input) (Stats, error) {
	result := Stats{}
	value, err := client.Query(ctx, input.Namespace, input.Name, input.Database, `SELECT pg_database_size(current_database()), (SELECT count(*) FROM pg_stat_activity), (SELECT count(*) FROM pg_stat_activity WHERE state='active' AND pid<>pg_backend_pid()), coalesce((SELECT sum(xact_commit+xact_rollback) FROM pg_stat_database),0)`)
	if err != nil {
		return result, err
	}
	parts := strings.Split(strings.TrimSpace(value), "|")
	if len(parts) != 4 {
		return result, errors.New("invalid PostgreSQL statistics response")
	}
	targets := []*int64{&result.DatabaseBytes, &result.Connections, &result.ActiveQueries, &result.Transactions}
	for i, part := range parts {
		*targets[i], err = strconv.ParseInt(part, 10, 64)
		if err != nil {
			return result, errors.New("invalid PostgreSQL statistic")
		}
	}
	primary, err := client.primary(ctx, input.Namespace, input.Name)
	if err != nil {
		return result, nil
	}
	disk, err := client.exec(ctx, input.Namespace, primary, []string{"sh", "-c", `df -Pk "$PGDATA" | tail -1`})
	if err != nil {
		return result, nil
	}
	fields := strings.Fields(disk)
	if len(fields) < 4 {
		return result, nil
	}
	capacity, e1 := strconv.ParseInt(fields[1], 10, 64)
	used, e2 := strconv.ParseInt(fields[2], 10, 64)
	available, e3 := strconv.ParseInt(fields[3], 10, 64)
	if errors.Join(e1, e2, e3) != nil {
		return result, nil
	}
	result.DiskAvailable = true
	result.DiskCapacityBytes = capacity * 1024
	result.DiskUsedBytes = used * 1024
	result.DiskAvailableBytes = available * 1024
	return result, nil
}

func (client Client) deleteOne(ctx context.Context, kind, namespace, name, owner string) error {
	handle := client.dynamic.Resource(resource(kind)).Namespace(namespace)
	current, err := handle.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if current.GetLabels()["borq.run/postgres-id"] != owner {
		return errors.New("refusing to delete PostgreSQL resource with conflicting ownership")
	}
	uid := current.GetUID()
	policy := metav1.DeletePropagationForeground
	return handle.Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}, PropagationPolicy: &policy})
}

// DeleteOwned removes only resources bearing the immutable resource identity and retains S3 objects.
func (client Client) DeleteOwned(ctx context.Context, input Input) (bool, error) {
	namespaces := []string{input.Namespace}
	if credentialNamespace(input) != input.Namespace {
		namespaces = append(namespaces, credentialNamespace(input))
	}
	pending := false
	for _, namespace := range namespaces {
		if !input.Isolated && input.EnvironmentExport.Name != "" {
			producer, err := client.dynamic.Resource(resource("PushSecret")).Namespace(namespace).Get(ctx, input.Name+"-environment", metav1.GetOptions{})
			if err != nil && !apierrors.IsNotFound(err) {
				return false, err
			}
			if err == nil && producer.GetLabels()["borq.run/postgres-id"] != input.Owner {
				return false, errors.New("refusing to delete PostgreSQL runtime with a conflicting environment producer")
			}
		}
		producers, err := client.dynamic.Resource(resource("PushSecret")).Namespace(namespace).List(ctx, metav1.ListOptions{LabelSelector: "borq.run/postgres-id=" + input.Owner})
		if err != nil && !apierrors.IsNotFound(err) {
			return false, err
		}
		if producers == nil {
			continue
		}
		for _, producer := range producers.Items {
			if err := client.deleteOne(ctx, "PushSecret", namespace, producer.GetName(), input.Owner); err != nil {
				return false, err
			}
			pending = true
		}
	}
	if pending {
		return false, nil
	}
	for _, kind := range []string{"ExternalSecret", "VaultDynamicSecret"} {
		for _, namespace := range namespaces {
			if err := client.deleteOne(ctx, kind, namespace, input.Name+"-credentials", input.Owner); err != nil {
				return false, err
			}
		}
	}
	if err := client.deleteCredentialSecret(ctx, credentialNamespace(input), input.Name+"-credentials", input.Owner); err != nil {
		return false, err
	}

	for _, kind := range []string{"CronJob", "ScheduledBackup", "Backup", "Cluster", "ObjectStore", "ConfigMap"} {
		objects, err := client.dynamic.Resource(resource(kind)).Namespace(input.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "borq.run/postgres-id=" + input.Owner})
		if err != nil {
			return false, err
		}
		for _, item := range objects.Items {
			if err := client.deleteOne(ctx, kind, input.Namespace, item.GetName(), input.Owner); err != nil {
				return false, err
			}
		}
	}
	cluster, err := client.dynamic.Resource(resource("Cluster")).Namespace(input.Namespace).Get(ctx, input.Name, metav1.GetOptions{})
	if err == nil && cluster.GetDeletionTimestamp() != nil {
		return false, nil
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}
	pods, err := client.kube.CoreV1().Pods(input.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "cnpg.io/cluster=" + input.Name})
	if err != nil {
		return false, err
	}
	if len(pods.Items) > 0 {
		return false, nil
	}
	claims, err := client.kube.CoreV1().PersistentVolumeClaims(input.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "cnpg.io/cluster=" + input.Name})
	if err != nil {
		return false, err
	}
	for _, claim := range claims.Items {
		if claim.Labels["borq.run/postgres-id"] != input.Owner {
			return false, errors.New("refusing to delete a PostgreSQL volume without matching immutable ownership")
		}
		uid := claim.UID
		if err := client.kube.CoreV1().PersistentVolumeClaims(input.Namespace).Delete(ctx, claim.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil && !apierrors.IsNotFound(err) {
			return false, err
		}
	}
	if len(claims.Items) > 0 {
		return false, nil
	}
	for _, name := range []string{input.Name + "-app", input.Name + "-s3"} {
		secret, err := client.kube.CoreV1().Secrets(input.Namespace).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return false, err
		}
		if secret.Labels["borq.run/postgres-id"] != input.Owner {
			return false, errors.New("credential Secret ownership conflict")
		}
		uid := secret.UID
		if err := client.kube.CoreV1().Secrets(input.Namespace).Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil && !apierrors.IsNotFound(err) {
			return false, err
		}
	}
	if err := client.deleteOne(ctx, "ServiceAccount", input.Namespace, input.Name+"-check", input.Owner); err != nil {
		return false, err
	}
	return true, nil
}

// PruneSnapshots expires CNPG snapshot backups using controller ownership and the configured retention window.
func (client Client) PruneSnapshots(ctx context.Context, input Input, retention time.Duration) error {
	backups, err := client.dynamic.Resource(resource("Backup")).Namespace(input.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	cluster, err := client.dynamic.Resource(resource("Cluster")).Namespace(input.Namespace).Get(ctx, input.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	for _, backup := range backups.Items {
		name, _, _ := unstructured.NestedString(backup.Object, "spec", "cluster", "name")
		method, _, _ := unstructured.NestedString(backup.Object, "spec", "method")
		if name != input.Name || method != "volumeSnapshot" || time.Since(backup.GetCreationTimestamp().Time) < retention {
			continue
		}
		owned := false
		for _, ref := range backup.GetOwnerReferences() {
			if ref.UID == cluster.GetUID() {
				owned = true
			}
			if ref.Kind == "ScheduledBackup" && ref.Name == input.Name+"-snapshots" {
				schedule, e := client.dynamic.Resource(resource("ScheduledBackup")).Namespace(input.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
				owned = e == nil && schedule.GetUID() == ref.UID && schedule.GetLabels()["borq.run/postgres-id"] == input.Owner
			}
		}
		if !owned {
			continue
		}
		uid := backup.GetUID()
		if err := client.dynamic.Resource(resource("Backup")).Namespace(input.Namespace).Delete(ctx, backup.GetName(), metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// AuthenticateCheck validates audience, account, bound Pod identity, and the owning live CronJob Job.
func (client Client) AuthenticateCheck(ctx context.Context, input Input, token, jobUID string) error {
	review, err := client.kube.AuthenticationV1().TokenReviews().Create(ctx, &authv1.TokenReview{Spec: authv1.TokenReviewSpec{Token: token, Audiences: []string{"borq-postgres-check"}}}, metav1.CreateOptions{})
	if err != nil {
		return errors.New("restore-check authentication unavailable")
	}
	if !review.Status.Authenticated || review.Status.User.Username != "system:serviceaccount:"+input.Namespace+":"+input.Name+"-check" {
		return errors.New("restore-check credentials do not match this resource")
	}
	audience := false
	for _, value := range review.Status.Audiences {
		if value == "borq-postgres-check" {
			audience = true
		}
	}
	if !audience {
		return errors.New("restore-check token audience mismatch")
	}
	names := review.Status.User.Extra["authentication.kubernetes.io/pod-name"]
	uids := review.Status.User.Extra["authentication.kubernetes.io/pod-uid"]
	if len(names) != 1 || len(uids) != 1 {
		return errors.New("restore-check requires a pod-bound token")
	}
	pod, err := client.kube.CoreV1().Pods(input.Namespace).Get(ctx, names[0], metav1.GetOptions{})
	if err != nil || string(pod.UID) != uids[0] || pod.Spec.ServiceAccountName != input.Name+"-check" {
		return errors.New("restore-check bound Pod is unavailable")
	}
	jobName := ""
	for _, owner := range pod.OwnerReferences {
		if owner.Kind == "Job" && string(owner.UID) == jobUID {
			jobName = owner.Name
		}
	}
	if jobName == "" {
		return errors.New("restore-check Job identity mismatch")
	}
	job, err := client.kube.BatchV1().Jobs(input.Namespace).Get(ctx, jobName, metav1.GetOptions{})
	if err != nil || string(job.UID) != jobUID {
		return errors.New("restore-check Job is unavailable")
	}
	cron, err := client.kube.BatchV1().CronJobs(input.Namespace).Get(ctx, input.Name+"-check", metav1.GetOptions{})
	if err != nil || cron.Labels["borq.run/postgres-id"] != input.Owner {
		return errors.New("restore-check CronJob is unavailable")
	}
	for _, owner := range job.OwnerReferences {
		if owner.Kind == "CronJob" && owner.UID == cron.UID {
			return nil
		}
	}
	return errors.New("restore-check Job is not owned by this resource")
}

// Logs reads bounded current-primary PostgreSQL logs when persisted telemetry is unavailable.
func (client Client) Logs(ctx context.Context, input Input, limit int, since time.Time) (string, error) {
	primary, err := client.primary(ctx, input.Namespace, input.Name)
	if err != nil {
		return "", err
	}
	lines := int64(limit)
	timestamps := true
	byteLimit := int64(1024 * 1024)
	options := corev1.PodLogOptions{Container: "postgres", TailLines: &lines, Timestamps: timestamps, LimitBytes: &byteLimit}
	if !since.IsZero() {
		stamp := metav1.NewTime(since)
		options.SinceTime = &stamp
	}
	stream, err := client.kube.CoreV1().Pods(input.Namespace).GetLogs(primary, &options).Stream(ctx)
	if err != nil {
		return "", err
	}
	defer stream.Close()
	value, err := io.ReadAll(io.LimitReader(stream, 1024*1024))
	return string(value), err
}

// RequestSnapshot creates an operation-owned, idempotent CSI backup request for recovery verification.
func (client Client) RequestSnapshot(ctx context.Context, input Input, operation string) error {
	manifest := object("postgresql.cnpg.io/v1", "Backup", input.Name+"-s-"+operation[:12], input.Namespace, input.Owner, map[string]any{"cluster": map[string]any{"name": input.Name}, "method": "volumeSnapshot", "online": true})
	return client.apply(ctx, manifest, input.Owner)
}

// CompletedSnapshot returns the operation's actual ready PGDATA snapshot after controller completion.
func (client Client) CompletedSnapshot(ctx context.Context, input Input, operation string) (string, error) {
	backup, err := client.dynamic.Resource(resource("Backup")).Namespace(input.Namespace).Get(ctx, input.Name+"-s-"+operation[:12], metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	if backup.GetLabels()["borq.run/postgres-id"] != input.Owner {
		return "", errors.New("CSI verification backup ownership conflict")
	}
	phase, _, _ := unstructured.NestedString(backup.Object, "status", "phase")
	if phase == "failed" {
		return "", errors.New("CSI snapshot backup failed")
	}
	if phase != "completed" {
		return "", nil
	}
	elements, _, _ := unstructured.NestedSlice(backup.Object, "status", "snapshotBackupStatus", "elements")
	for _, raw := range elements {
		element, _ := raw.(map[string]any)
		if element["type"] != "PG_DATA" {
			continue
		}
		name, _ := element["name"].(string)
		snapshot, err := client.dynamic.Resource(resource("VolumeSnapshot")).Namespace(input.Namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		owned := false
		for _, owner := range snapshot.GetOwnerReferences() {
			if owner.Kind == "Backup" && owner.UID == backup.GetUID() {
				owned = true
			}
		}
		if !owned {
			return "", errors.New("CSI recovery snapshot ownership conflict")
		}
		ready, _, _ := unstructured.NestedBool(snapshot.Object, "status", "readyToUse")
		if ready {
			return name, nil
		}
	}
	return "", nil
}

// DeleteSnapshotRequest removes only this verification's CSI Backup and its controller-owned snapshot.
func (client Client) DeleteSnapshotRequest(ctx context.Context, input Input, operation string) error {
	return client.deleteOne(ctx, "Backup", input.Namespace, input.Name+"-s-"+operation[:12], input.Owner)
}

// CleanupSnapshotRequest removes the verification request and waits until its CSI snapshots are gone.
func (client Client) CleanupSnapshotRequest(ctx context.Context, input Input, operation string) (bool, error) {
	name := input.Name + "-s-" + operation[:12]
	if err := client.DeleteSnapshotRequest(ctx, input, operation); err != nil {
		return false, err
	}
	_, err := client.dynamic.Resource(resource("Backup")).Namespace(input.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		return false, nil
	}
	if !apierrors.IsNotFound(err) {
		return false, err
	}
	snapshots, err := client.dynamic.Resource(resource("VolumeSnapshot")).Namespace(input.Namespace).List(ctx, metav1.ListOptions{})
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	for _, snapshot := range snapshots.Items {
		for _, owner := range snapshot.GetOwnerReferences() {
			if owner.Kind == "Backup" && owner.Name == name {
				return false, nil
			}
		}
	}
	return true, nil
}
