package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/matryer/is"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
)

// TestArchiveSwitchWaitsForAdmittedBackups proves the write guard against an unfinished old backup.
// The clients supply admitted resources only; no controller completion or recovery is simulated.
func TestArchiveSwitchWaitsForAdmittedBackups(t *testing.T) {
	for _, test := range []struct {
		name                    string
		detached, updatedPlugin bool
	}{
		{name: "failed disable then enable"},
		{name: "detached plugin with admitted backup", detached: true},
		{name: "plugin updated before store", updatedPlugin: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			check := is.New(t)
			input := Input{Namespace: "space", Name: "pg-database", Owner: "database", BackupEnabled: true, BackupSchedule: "0 0 0 * * *", SnapshotClass: "snapshots", SnapshotSchedule: "0 0 * * * *", Storage: Storage{Bucket: "backups", Prefix: "old-prefix", ServerName: "archive-old"}, Check: Check{Enabled: true, Schedule: "0 3 * * 0", ApiURL: "https://borq.test/check", TimeoutSeconds: 1800}}
			objects := []runtime.Object{}
			for _, manifest := range Manifests(input) {
				kind := manifest["kind"]
				if kind == "Cluster" && test.detached {
					manifest["spec"].(map[string]any)["plugins"] = []any{}
				}
				if kind == "Cluster" && test.updatedPlugin {
					plugin := manifest["spec"].(map[string]any)["plugins"].([]any)[0].(map[string]any)
					plugin["parameters"].(map[string]any)["serverName"] = "archive-new"
				}
				if kind == "Cluster" || kind == "ObjectStore" || kind == "ScheduledBackup" {
					objects = append(objects, &unstructured.Unstructured{Object: manifest})
				}
			}
			pending := object("postgresql.cnpg.io/v1", "Backup", "admitted", input.Namespace, input.Owner, map[string]any{"cluster": map[string]any{"name": input.Name}, "method": "plugin"})
			pending["status"] = map[string]any{"phase": "running"}
			objects = append(objects, &unstructured.Unstructured{Object: pending})
			dynamic := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{resource("ScheduledBackup"): "ScheduledBackupList", resource("Backup"): "BackupList"}, objects...)
			kube := kubefake.NewSimpleClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: input.Namespace}}, &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: input.Name + "-check", Namespace: input.Namespace, Labels: map[string]string{"borq.run/postgres-id": input.Owner}}})
			client := Client{dynamic: dynamic, kube: kube}
			input.Storage.Prefix, input.Storage.ServerName = "new-prefix", "archive-new"
			ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
			defer cancel()
			check.True(errors.Is(client.Reconcile(ctx, input), context.DeadlineExceeded))
			store, err := dynamic.Resource(resource("ObjectStore")).Namespace(input.Namespace).Get(t.Context(), input.Name+"-store", metav1.GetOptions{})
			check.NoErr(err)
			destination, _, err := unstructured.NestedString(store.Object, "spec", "configuration", "destinationPath")
			check.NoErr(err)
			check.Equal(destination, "s3://backups/old-prefix")
			schedules, err := dynamic.Resource(resource("ScheduledBackup")).Namespace(input.Namespace).List(t.Context(), metav1.ListOptions{})
			check.NoErr(err)
			for _, schedule := range schedules.Items {
				suspended, _, err := unstructured.NestedBool(schedule.Object, "spec", "suspend")
				check.NoErr(err)
				check.True(suspended)
			}
			cron, err := kube.BatchV1().CronJobs(input.Namespace).Get(t.Context(), input.Name+"-check", metav1.GetOptions{})
			check.NoErr(err)
			check.True(cron.Spec.Suspend != nil && *cron.Spec.Suspend)
			backup, err := dynamic.Resource(resource("Backup")).Namespace(input.Namespace).Get(t.Context(), "admitted", metav1.GetOptions{})
			check.NoErr(err)
			phase, _, err := unstructured.NestedString(backup.Object, "status", "phase")
			check.NoErr(err)
			check.Equal(phase, "running")
		})
	}
}

// TestBackupHealth classifies controller readback records; seeded phases do not prove backup execution.
func TestBackupHealth(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	input := Input{Name: "pg-database", BackupSchedule: "0 0 0 * * *", Storage: Storage{ServerName: "archive-current"}}
	record := func(phase string, stopped time.Time, current bool) unstructured.Unstructured {
		archive := "archive-current"
		if !current {
			archive = "archive-old"
		}
		return unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"labels": map[string]any{"borq.run/postgres-archive": archive}},
			"spec":     map[string]any{"cluster": map[string]any{"name": input.Name}, "method": "plugin"},
			"status":   map[string]any{"phase": phase, "stoppedAt": stopped.Format(time.RFC3339Nano)},
		}}
	}
	success := record("completed", now.Add(-2*time.Hour), true)
	failed := record("failed", now.Add(-time.Hour), true)
	pending := record("running", now.Add(-time.Minute), true)
	noStopped := record("failed", now.Add(-time.Hour), true)
	delete(noStopped.Object["status"].(map[string]any), "stoppedAt")
	noStopped.Object["status"].(map[string]any)["reconciliationTerminatedAt"] = now.Add(-time.Hour).Format(time.RFC3339Nano)
	createdFailure := noStopped.DeepCopy()
	delete(createdFailure.Object["status"].(map[string]any), "reconciliationTerminatedAt")
	createdFailure.SetCreationTimestamp(metav1.NewTime(now.Add(-time.Hour)))
	for _, test := range []struct {
		name, status string
		backups      []unstructured.Unstructured
		latest       time.Time
	}{
		{name: "fresh success", status: "ready", backups: []unstructured.Unstructured{success}, latest: now.Add(-2 * time.Hour)},
		{name: "later failure", status: "failed", backups: []unstructured.Unstructured{success, failed}, latest: now.Add(-2 * time.Hour)},
		{name: "failure without tool stop", status: "failed", backups: []unstructured.Unstructured{success, noStopped}, latest: now.Add(-2 * time.Hour)},
		{name: "failure creation fallback", status: "failed", backups: []unstructured.Unstructured{*createdFailure, success}, latest: now.Add(-2 * time.Hour)},
		{name: "pending cannot clear failure", status: "failed", backups: []unstructured.Unstructured{success, failed, pending}, latest: now.Add(-2 * time.Hour)},
		{name: "later success clears failure", status: "ready", backups: []unstructured.Unstructured{failed, record("completed", now.Add(-time.Minute), true), success}, latest: now.Add(-time.Minute)},
		{name: "other archive ignored", status: "ready", backups: []unstructured.Unstructured{success, record("failed", now.Add(-time.Minute), false)}, latest: now.Add(-2 * time.Hour)},
		{name: "cadence grace elapsed", status: "failed", backups: []unstructured.Unstructured{record("completed", now.Add(-26*time.Hour), true), pending}, latest: now.Add(-26 * time.Hour)},
		{name: "pending without success", status: "pending", backups: []unstructured.Unstructured{pending}},
	} {
		t.Run(test.name, func(t *testing.T) {
			check := is.New(t)
			status, latest := backupHealth(input, test.backups, now)
			check.Equal(status, test.status)
			check.True(latest.Equal(test.latest))
		})
	}
	// The schedule is six-field UTC, with a bounded transfer grace after the next due time.
	input.BackupSchedule = "0 0 * * * *"
	recent := record("completed", now.Add(-30*time.Minute), true)
	status, _ := backupHealth(input, []unstructured.Unstructured{recent}, now.Add(time.Hour))
	is.New(t).Equal(status, "ready")
	status, _ = backupHealth(input, []unstructured.Unstructured{recent}, now.Add(time.Hour+time.Second))
	is.New(t).Equal(status, "failed")
}

// TestPrimaryPolicyReadback classifies real Pod-shaped readback, independently of stale Cluster conditions.
// It does not simulate plugin startup, SQL, archival, or recovery success.
func TestPrimaryPolicyReadback(t *testing.T) {
	started, always := true, corev1.ContainerRestartPolicyAlways
	input := Input{Name: "pg-database", BackupEnabled: true, Storage: Storage{ServerName: "archive-current"}}
	configured := unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"plugins": []any{map[string]any{"name": "barman-cloud.cloudnative-pg.io", "isWALArchiver": true, "parameters": map[string]any{"serverName": input.Storage.ServerName, "barmanObjectName": input.Name + "-store"}}}}}}
	plugin := corev1.Container{Name: "plugin-barman-cloud", RestartPolicy: &always, StartupProbe: &corev1.Probe{}}
	pluginStatus := corev1.ContainerStatus{Name: plugin.Name, Started: &started, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}
	pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"cnpg.io/cluster": input.Name}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "postgres", Env: []corev1.EnvVar{{Name: "BORQ_BACKUP_ARCHIVE", Value: input.Storage.ServerName}}}}, InitContainers: []corev1.Container{plugin}}, Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}, ContainerStatuses: []corev1.ContainerStatus{{Name: "postgres", Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}, InitContainerStatuses: []corev1.ContainerStatus{pluginStatus}}}
	for _, test := range []struct {
		name                                           string
		enabled, recovery, declared, running, expected bool
	}{
		{name: "old primary before enable", enabled: true, expected: false},
		{name: "sidecar waiting on startup", enabled: true, declared: true, expected: false},
		{name: "enabled primary", enabled: true, declared: true, running: true, expected: true},
		{name: "old archiver before disable", declared: true, running: true, expected: false},
		{name: "disabled primary", expected: true},
		{name: "source-only recovery", recovery: true, declared: true, running: true, expected: true},
		{name: "completed source-only recovery without sidecar", recovery: true, expected: true},
		{name: "source-only recovery sidecar starting", recovery: true, declared: true, expected: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			check := is.New(t)
			current := *pod.DeepCopy()
			currentInput := input
			currentInput.BackupEnabled = test.enabled
			current.Spec.Containers[0].Env[0].Value = backupPolicyMarker(currentInput)
			if test.recovery {
				currentInput.Recovery = &Recovery{BackupID: "source"}
			}
			cluster := *configured.DeepCopy()
			if !test.enabled {
				cluster.Object["spec"].(map[string]any)["plugins"] = []any{}
			}
			if !test.declared {
				current.Spec.InitContainers = nil
			}
			if !test.running {
				current.Status.InitContainerStatuses = nil
			}
			check.Equal(primaryPolicyReady(cluster, current, currentInput), test.expected)
		})
	}
	stale := *pod.DeepCopy()
	stale.Spec.Containers[0].Env[0].Value = "archive-old"
	is.New(t).True(!primaryPolicyReady(configured, stale, input))
	wrong := *configured.DeepCopy()
	plugins := wrong.Object["spec"].(map[string]any)["plugins"].([]any)
	plugins[0].(map[string]any)["parameters"].(map[string]any)["serverName"] = "archive-old"
	is.New(t).True(!primaryPolicyReady(wrong, pod, input))
}
