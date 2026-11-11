package apps

import (
	"fmt"
	"path"
	"reflect"
	"slices"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

func inspectSqlite(pod corev1.PodSpec, container corev1.Container, replication bool) (SqliteSettings, []corev1.EnvVar, error) {
	settings := SqliteSettings{}
	variables := []corev1.EnvVar{}
	for _, variable := range container.Env {
		if variable.Name != sqlitePathVariable {
			variables = append(variables, variable)
			continue
		}
		if settings.Enabled || variable.Value != sqliteDatabasePath || variable.ValueFrom != nil {
			return settings, nil, fmt.Errorf("SQLite requires its fixed SQLITE_DATABASE_PATH")
		}
		settings.Enabled = true
	}
	if !settings.Enabled {
		return settings, variables, nil
	}
	if pod.SecurityContext == nil || pod.SecurityContext.FSGroup == nil || *pod.SecurityContext.FSGroup != 2000 {
		return settings, nil, fmt.Errorf("SQLite requires the shared database filesystem group")
	}
	if !slices.Contains(container.VolumeMounts, corev1.VolumeMount{Name: "borq-sqlite", MountPath: sqliteDirectory}) {
		return settings, nil, fmt.Errorf("SQLite requires its writable database directory mount")
	}
	volumeFound := false
	for _, volume := range pod.Volumes {
		if volume.Name == "borq-sqlite" {
			if volume.PersistentVolumeClaim == nil || volume.PersistentVolumeClaim.ClaimName != container.Name+"-sqlite" || volume.PersistentVolumeClaim.ReadOnly {
				return settings, nil, fmt.Errorf("SQLite requires its application-owned persistent volume")
			}
			volumeFound = true
		}
		if volume.Name == "borq-litestream-config" {
			if volume.ConfigMap == nil || volume.ConfigMap.Name != container.Name+"-litestream" {
				return settings, nil, fmt.Errorf("SQLite backups require their application-owned configuration")
			}
			settings.Backups = true
		}
	}
	if !volumeFound {
		return settings, nil, fmt.Errorf("SQLite database volume is missing")
	}
	prepared, restored, replicated := false, false, false
	for _, initialization := range pod.InitContainers {
		if initialization.Name == "sqlite-restore" {
			if initialization.Image != litestreamImage || !reflect.DeepEqual(initialization.Command, []string{"/bin/sh", "-ec"}) || len(initialization.Args) != 1 || !strings.Contains(initialization.Args[0], "litestream restore -config /etc/litestream/litestream.yml -if-replica-exists -o") {
				return settings, nil, fmt.Errorf("SQLite requires staged restoration")
			}
			restored = true
		}
		if initialization.Name == "sqlite-prepare" {
			if initialization.Image != sqliteRecoveryImage || !reflect.DeepEqual(initialization.Command, []string{"python", "/etc/borq-sqlite/sqlite-prepare.py"}) || len(initialization.Args) != 4 || initialization.Args[0] != "prepare" || initialization.Args[1] != sqliteDatabasePath || initialization.Args[3] != container.Name+"-sqlite-state" || !slices.Contains(initialization.VolumeMounts, corev1.VolumeMount{Name: "borq-sqlite", MountPath: sqliteDirectory}) {
				return settings, nil, fmt.Errorf("SQLite requires its database initialization container")
			}
			if settings.Backups && !restored {
				return settings, nil, fmt.Errorf("SQLite backups require restore before application startup")
			}
			prepared = true
		}
		if initialization.Name == "litestream" && replication {
			if !prepared || initialization.Image != litestreamImage || initialization.RestartPolicy == nil || *initialization.RestartPolicy != corev1.ContainerRestartPolicyAlways ||
				!reflect.DeepEqual(initialization.Command, []string{"litestream"}) || !reflect.DeepEqual(initialization.Args, []string{"replicate", "-config", "/etc/litestream/litestream.yml"}) ||
				!slices.Contains(initialization.VolumeMounts, corev1.VolumeMount{Name: "borq-sqlite", MountPath: sqliteDirectory}) {
				return settings, nil, fmt.Errorf("SQLite backups require their native Litestream sidecar after restoration")
			}
			replicated = true
		}
	}
	if !prepared || replication && settings.Backups != replicated {
		return settings, nil, fmt.Errorf("SQLite initialization or replication container is missing")
	}
	return settings, variables, nil
}

func inspectSqliteResource(resource inspectedResource, application App) error {
	if !application.Sqlite.Enabled {
		return fmt.Errorf("SQLite resource has no enabled application")
	}
	if resource.kind == "PersistentVolumeClaim" {
		claim := corev1.PersistentVolumeClaim{}
		if err := decodeManifest(resource.manifest, &claim); err != nil {
			return err
		}
		if claim.Name != application.Name+"-sqlite" || !reflect.DeepEqual(claim.Spec.AccessModes, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}) {
			return fmt.Errorf("SQLite requires its single-node persistent volume claim")
		}
		return nil
	}
	configuration := corev1.ConfigMap{}
	if err := decodeManifest(resource.manifest, &configuration); err != nil {
		return err
	}
	if configuration.Name == application.Name+"-sqlite-recovery" {
		script, err := chartFiles.ReadFile("charts/app/files/sqlite-prepare.py")
		if err != nil {
			return err
		}
		if strings.TrimSpace(configuration.Data["sqlite-prepare.py"]) != strings.TrimSpace(string(script)) {
			return fmt.Errorf("SQLite recovery verification script cannot be represented")
		}
		return nil
	}
	if !application.Sqlite.Backups || configuration.Name != application.Name+"-litestream" || configuration.Data["litestream.yml"] == "" {
		return fmt.Errorf("SQLite backup configuration does not match its application")
	}
	var backup sqliteBackupConfig
	if err := yaml.UnmarshalStrict([]byte(configuration.Data["litestream.yml"]), &backup); err != nil {
		return fmt.Errorf("SQLite backup configuration has unsupported fields")
	}
	if len(backup.Databases) != 1 || backup.Databases[0].Path != sqliteDatabasePath || len(backup.Databases[0].Replicas) != 1 {
		return fmt.Errorf("SQLite backups require the fixed database and one replica")
	}
	replica := backup.Databases[0].Replicas[0]
	suffix := path.Join("sqlite", resource.target, application.Name)
	if !slices.Equal(replica.Age.Identities, []string{"${LITESTREAM_AGE_IDENTITY}"}) || !slices.Equal(replica.Age.Recipients, []string{"${LITESTREAM_AGE_RECIPIENT}"}) || replica.Type != "s3" || replica.Bucket == "" || replica.Region == "" || replica.Retention != "168h" || replica.SnapshotInterval != "24h" || replica.SyncInterval != "" || !(replica.Path == suffix || strings.HasSuffix(replica.Path, "/"+suffix)) || path.Clean(replica.Path) != replica.Path {
		return fmt.Errorf("SQLite backup destination or policy cannot be represented")
	}
	return nil
}

func inspectSqliteAuxiliary(resource inspectedResource, application App) error {
	if !application.Sqlite.Enabled {
		return fmt.Errorf("SQLite recovery resource has no enabled application")
	}
	name := application.Name + "-sqlite"
	switch resource.kind {
	case "ServiceAccount":
		var account corev1.ServiceAccount
		if err := decodeManifest(resource.manifest, &account); err != nil {
			return err
		}
		if account.Name != name || account.AutomountServiceAccountToken == nil || *account.AutomountServiceAccountToken {
			return fmt.Errorf("SQLite initialization requires its restricted service account")
		}
	case "Role":
		var role rbacv1.Role
		if err := decodeManifest(resource.manifest, &role); err != nil {
			return err
		}
		expected := []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"configmaps"}, ResourceNames: []string{application.Name + "-sqlite-state"}, Verbs: []string{"get", "patch"}}}
		if role.Name != name || !reflect.DeepEqual(role.Rules, expected) {
			return fmt.Errorf("SQLite initialization role must access only its own state")
		}
	case "RoleBinding":
		var binding rbacv1.RoleBinding
		if err := decodeManifest(resource.manifest, &binding); err != nil {
			return err
		}
		if binding.Name != name || binding.RoleRef != (rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: name}) || !reflect.DeepEqual(binding.Subjects, []rbacv1.Subject{{Kind: "ServiceAccount", Name: name, Namespace: binding.Namespace}}) {
			return fmt.Errorf("SQLite initialization binding must use its own role and service account")
		}
	case "CronJob":
		var check batchv1.CronJob
		if err := decodeManifest(resource.manifest, &check); err != nil {
			return err
		}
		if check.Name != sqliteCheckName(application.Name) || check.Spec.Schedule != "0 3 * * 0" || check.Spec.ConcurrencyPolicy != batchv1.ForbidConcurrent || check.Spec.Suspend == nil || *check.Spec.Suspend == application.Sqlite.Backups {
			return fmt.Errorf("SQLite recovery verification policy cannot be represented")
		}
		pod := check.Spec.JobTemplate.Spec.Template.Spec
		if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken || len(pod.InitContainers) != 1 || len(pod.Containers) != 1 {
			return fmt.Errorf("SQLite recovery verification requires isolated restore and integrity containers")
		}
		restore, verify := pod.InitContainers[0], pod.Containers[0]
		if restore.Image != litestreamImage || !reflect.DeepEqual(restore.Command, []string{"litestream"}) || !reflect.DeepEqual(restore.Args, []string{"restore", "-config", "/etc/litestream/litestream.yml", "-o", sqliteDatabasePath + ".restoring", sqliteDatabasePath}) ||
			verify.Image != sqliteRecoveryImage || !reflect.DeepEqual(verify.Command, []string{"python", "/etc/borq-sqlite/sqlite-prepare.py"}) || !reflect.DeepEqual(verify.Args, []string{"verify", sqliteDatabasePath, "", ""}) {
			return fmt.Errorf("SQLite recovery verification must restore an existing backup and check its integrity")
		}
		for _, volume := range pod.Volumes {
			if volume.PersistentVolumeClaim != nil || volume.Projected != nil {
				return fmt.Errorf("SQLite recovery verification cannot mount live database storage or API tokens")
			}
		}
	}
	return nil
}
