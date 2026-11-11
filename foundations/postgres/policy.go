package postgres

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

func backupScheduleName(input Input) string {
	server := input.Storage.ServerName
	if len(server) > 12 {
		server = server[len(server)-12:]
	}
	return input.Name + "-daily-" + server
}

func backupInArchive(backup unstructured.Unstructured, input Input) bool {
	if input.Storage.ServerName != "" && backup.GetLabels()["borq.run/postgres-archive"] == input.Storage.ServerName {
		return true
	}
	for _, owner := range backup.GetOwnerReferences() {
		if owner.Kind == "ScheduledBackup" && owner.Name == backupScheduleName(input) {
			return true
		}
	}
	return false
}

func (client Client) archiveSwitchRequired(ctx context.Context, input Input) (bool, error) {
	if input.Isolated || !input.BackupEnabled {
		return false, nil
	}
	cluster, err := client.dynamic.Resource(resource("Cluster")).Namespace(input.Namespace).Get(ctx, input.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if cluster.GetLabels()["borq.run/postgres-id"] != input.Owner {
		return false, errors.New("database has conflicting ownership")
	}
	plugins, _, err := unstructured.NestedSlice(cluster.Object, "spec", "plugins")
	if err != nil {
		return false, err
	}
	matchingPlugin := false
	for _, raw := range plugins {
		plugin, _ := raw.(map[string]any)
		if plugin["name"] != "barman-cloud.cloudnative-pg.io" {
			continue
		}
		server, _, _ := unstructured.NestedString(plugin, "parameters", "serverName")
		store, _, _ := unstructured.NestedString(plugin, "parameters", "barmanObjectName")
		matchingPlugin = server == input.Storage.ServerName && store == input.Name+"-store"
	}
	if !matchingPlugin {
		return true, nil
	}
	store, err := client.dynamic.Resource(resource("ObjectStore")).Namespace(input.Namespace).Get(ctx, input.Name+"-store", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if store.GetLabels()["borq.run/postgres-id"] != input.Owner {
		return false, errors.New("backup storage has conflicting ownership")
	}
	destination, _, err := unstructured.NestedString(store.Object, "spec", "configuration", "destinationPath")
	if err != nil {
		return false, err
	}
	return destination != "s3://"+input.Storage.Bucket+"/"+strings.Trim(input.Storage.Prefix, "/"), nil
}

func (client Client) suspendBackupSchedules(ctx context.Context, input Input) error {
	schedules, err := client.dynamic.Resource(resource("ScheduledBackup")).Namespace(input.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, schedule := range schedules.Items {
		cluster, _, _ := unstructured.NestedString(schedule.Object, "spec", "cluster", "name")
		if cluster != input.Name {
			continue
		}
		if schedule.GetLabels()["borq.run/postgres-id"] != input.Owner {
			return errors.New("backup schedule has conflicting ownership")
		}
		keep := input.BackupEnabled && !input.Isolated && (schedule.GetName() == backupScheduleName(input) || schedule.GetName() == input.Name+"-snapshots" && input.SnapshotClass != "")
		if keep {
			continue
		}
		if _, err := client.dynamic.Resource(resource("ScheduledBackup")).Namespace(input.Namespace).Patch(ctx, schedule.GetName(), types.MergePatchType, []byte(`{"spec":{"suspend":true}}`), metav1.PatchOptions{FieldManager: "borq-postgres"}); err != nil {
			return err
		}
	}
	if input.BackupEnabled && !input.Isolated {
		return nil
	}
	cron, err := client.kube.BatchV1().CronJobs(input.Namespace).Get(ctx, input.Name+"-check", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if cron.Labels["borq.run/postgres-id"] != input.Owner {
		return errors.New("restore-test schedule has conflicting ownership")
	}
	suspended := true
	cron.Spec.Suspend = &suspended
	_, err = client.kube.BatchV1().CronJobs(input.Namespace).Update(ctx, cron, metav1.UpdateOptions{FieldManager: "borq-postgres"})
	return err
}

func (client Client) backupWorkSettled(ctx context.Context, input Input) (bool, error) {
	backups, err := client.dynamic.Resource(resource("Backup")).Namespace(input.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, err
	}
	for _, backup := range backups.Items {
		cluster, _, _ := unstructured.NestedString(backup.Object, "spec", "cluster", "name")
		phase, _, _ := unstructured.NestedString(backup.Object, "status", "phase")
		if cluster == input.Name && phase != "completed" && phase != "failed" {
			return false, nil
		}
	}
	jobs, err := client.kube.BatchV1().Jobs(input.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, err
	}
	for _, job := range jobs.Items {
		owned := false
		for _, owner := range job.OwnerReferences {
			if owner.Kind == "CronJob" && owner.Name == input.Name+"-check" {
				owned = true
			}
		}
		if !owned {
			continue
		}
		settled := false
		for _, condition := range job.Status.Conditions {
			if (condition.Type == batchv1.JobComplete || condition.Type == batchv1.JobFailed) && condition.Status == "True" {
				settled = true
			}
		}
		if !settled {
			return false, nil
		}
	}
	return true, nil
}

// RequestCheck creates an idempotent real Job from the resource's authenticated restore-test CronJob.
func (client Client) RequestCheck(ctx context.Context, input Input, operation string) error {
	if !input.BackupEnabled || input.Check.ApiURL == "" {
		return errors.New("restore-test routing is unavailable")
	}
	cron, err := client.kube.BatchV1().CronJobs(input.Namespace).Get(ctx, input.Name+"-check", metav1.GetOptions{})
	if err != nil {
		return err
	}
	if cron.Labels["borq.run/postgres-id"] != input.Owner {
		return errors.New("restore-test schedule has conflicting ownership")
	}
	job := batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: input.Name + "-check-" + operation[:12], Namespace: input.Namespace, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(cron, schema.GroupVersionKind{Group: "batch", Version: "v1", Kind: "CronJob"})}}, Spec: *cron.Spec.JobTemplate.Spec.DeepCopy()}
	existing, err := client.kube.BatchV1().Jobs(input.Namespace).Get(ctx, job.Name, metav1.GetOptions{})
	if err == nil {
		for _, owner := range existing.OwnerReferences {
			if owner.UID == cron.UID && owner.Kind == "CronJob" {
				return nil
			}
		}
		return errors.New("restore-test Job has conflicting ownership")
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	_, err = client.kube.BatchV1().Jobs(input.Namespace).Create(ctx, &job, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func backupHealth(input Input, backups []unstructured.Unstructured, now time.Time) (string, time.Time) {
	status := "pending"
	latestOutcome, lastBackup := time.Time{}, time.Time{}
	for _, backup := range backups {
		cluster, _, _ := unstructured.NestedString(backup.Object, "spec", "cluster", "name")
		method, _, _ := unstructured.NestedString(backup.Object, "spec", "method")
		phase, _, _ := unstructured.NestedString(backup.Object, "status", "phase")
		if cluster != input.Name || method != "plugin" || !backupInArchive(backup, input) || (phase != "completed" && phase != "failed") {
			continue
		}
		stopped, _, _ := unstructured.NestedString(backup.Object, "status", "stoppedAt")
		completedAt, completedErr := time.Parse(time.RFC3339Nano, stopped)
		outcomeAt := backup.GetCreationTimestamp().Time
		// CNPG SetAsFailed records reconciliationTerminatedAt even when the backup tool never stopped.
		for _, field := range []string{"reconciliationTerminatedAt", "stoppedAt", "startedAt", "reconciliationStartedAt"} {
			value, _, _ := unstructured.NestedString(backup.Object, "status", field)
			if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
				outcomeAt = parsed
				break
			}
		}
		if phase == "completed" && completedErr == nil && completedAt.After(lastBackup) {
			lastBackup = completedAt
		}
		if outcomeAt.After(latestOutcome) || (outcomeAt.Equal(latestOutcome) && phase == "failed") {
			latestOutcome = outcomeAt
			status = "ready"
			if phase == "failed" {
				status = "failed"
			}
		}
	}
	if status != "ready" {
		return status, lastBackup
	}
	if lastBackup.IsZero() {
		return "pending", lastBackup
	}
	schedule, err := cron.NewParser(cron.Second | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow).Parse(input.BackupSchedule)
	if err != nil {
		return "failed", lastBackup
	}
	// One hour allows controller scheduling and object transfer without treating retention as freshness.
	due := schedule.Next(lastBackup)
	if due.IsZero() || now.After(due.Add(time.Hour)) {
		return "failed", lastBackup
	}
	return "ready", lastBackup
}

// ActivateBackupSchedules starts future triggers only after the accepted policy reaches the primary.
func (client Client) ActivateBackupSchedules(ctx context.Context, input Input) error {
	if !input.BackupEnabled || input.Isolated {
		return nil
	}
	for _, manifest := range Manifests(input) {
		if manifest["kind"] != "ScheduledBackup" && manifest["kind"] != "CronJob" {
			continue
		}
		manifest["spec"].(map[string]any)["suspend"] = false
		if err := client.apply(ctx, manifest, input.Owner); err != nil {
			return err
		}
	}
	return nil
}

func backupPolicyMarker(input Input) string {
	if input.BackupEnabled && !input.Isolated {
		return input.Storage.ServerName
	}
	return "disabled"
}

func primaryPolicyReady(cluster unstructured.Unstructured, pod corev1.Pod, input Input) bool {
	if pod.DeletionTimestamp != nil || pod.Labels["cnpg.io/cluster"] != input.Name {
		return false
	}
	// Pod env is immutable, so this identifies the primary created for the accepted archive policy.
	policyApplied := false
	for _, container := range pod.Spec.Containers {
		if container.Name != "postgres" {
			continue
		}
		for _, variable := range container.Env {
			if variable.Name == "BORQ_BACKUP_ARCHIVE" && variable.Value == backupPolicyMarker(input) {
				policyApplied = true
			}
		}
	}
	if !policyApplied {
		return false
	}
	postgresReady, podReady := false, false
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == "postgres" && status.Ready && status.State.Running != nil {
			postgresReady = true
		}
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			podReady = true
		}
	}
	if !postgresReady || !podReady {
		return false
	}
	outgoing := false
	plugins, _, _ := unstructured.NestedSlice(cluster.Object, "spec", "plugins")
	for _, raw := range plugins {
		plugin, _ := raw.(map[string]any)
		if plugin["name"] != "barman-cloud.cloudnative-pg.io" {
			continue
		}
		server, _, _ := unstructured.NestedString(plugin, "parameters", "serverName")
		store, _, _ := unstructured.NestedString(plugin, "parameters", "barmanObjectName")
		if !input.BackupEnabled || server != input.Storage.ServerName || store != input.Name+"-store" || plugin["isWALArchiver"] != true {
			return false
		}
		outgoing = true
	}
	if outgoing != input.BackupEnabled {
		return false
	}
	needsPlugin := input.BackupEnabled || input.Recovery != nil
	declared, started := false, false
	for _, container := range pod.Spec.InitContainers {
		if container.Name != "plugin-barman-cloud" {
			continue
		}
		declared = true
		if container.RestartPolicy == nil || *container.RestartPolicy != corev1.ContainerRestartPolicyAlways || container.StartupProbe == nil {
			return false
		}
	}
	for _, status := range pod.Status.InitContainerStatuses {
		if status.Name != "plugin-barman-cloud" {
			continue
		}
		if !needsPlugin {
			return false
		}
		started = status.Started != nil && *status.Started && status.State.Running != nil
	}
	if !declared {
		// Source-only recovery can finish in the bootstrap Job without a sidecar on the primary.
		return !input.BackupEnabled
	}
	// Restartable sidecar Started is set only after its configured socket startup probe succeeds.
	return needsPlugin && started
}
