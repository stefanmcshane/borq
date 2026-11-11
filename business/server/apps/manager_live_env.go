package apps

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/stefanmcshane/borq/foundations/kubeapps"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

func (manager Manager) refreshManagedEnvironmentConsumers(ctx context.Context, organization, space, groupName, postgresID string) (bool, error) {
	manager.runtime.mutex.Lock()
	defer manager.runtime.mutex.Unlock()
	key := spaceKey(organization, space)
	stored, record, err := loadDocument[spaceRecord](ctx, manager.database, "space", key)
	if err != nil {
		return false, err
	}
	original, err := json.Marshal(stored)
	if err != nil {
		return false, err
	}
	if stored.ActiveOperation != "" || len(stored.Queue) != 0 {
		return false, nil
	}
	group, _, err := loadDocument[environmentGroupRecord](ctx, manager.database, "environment-group", stored.Space.ID+"/"+groupName)
	if err != nil {
		return false, err
	}
	if group.PostgresID != postgresID || postgresID == "" {
		return false, fmt.Errorf("%w: managed environment producer changed", ErrConflict)
	}
	_, client, err := manager.environmentGroupConnection(ctx, EnvironmentGroupTarget{Organization: organization, Space: space})
	if err != nil {
		return false, err
	}
	complete, changed := true, false
	for index, application := range stored.Space.Configuration.Apps {
		if application.Env.Name != groupName {
			continue
		}
		vault, mount, err := openEnvironmentVault(ctx, client)
		if err != nil {
			return false, err
		}
		snapshot := managedEnvSnapshot(stored.Space.ID, application.Name, group)
		current, err := vault.ReadKv(ctx, mount, snapshot.Path, 0)
		if err != nil {
			return false, err
		}
		previousSecret := ""
		for _, previous := range stored.EnvSnapshots {
			if previous.AppName == application.Name {
				previousSecret = EnvSecretName(previous)
			}
		}
		revision := ""
		for _, manifest := range stored.Manifests {
			if manifest["kind"] == "Deployment" && manifestText(manifest, "metadata", "name") == application.Name {
				revision = manifestText(manifest, "spec", "template", "metadata", "labels", "borq.run/revision")
			}
		}
		if revision == "" {
			return false, fmt.Errorf("accepted application deployment revision is unavailable")
		}
		application.Env = EnvReference{Name: groupName, Version: "latest"}
		rendered, err := RenderWithEnv(Configuration{Apps: []App{application}}, stored.Space.Namespace, stored.Space.ID, "credential-refresh", stored.Space.CertificateIssuer, []EnvSnapshot{snapshot}, ResourceScope{Organization: organization, Space: space, Cluster: stored.Space.Cluster})
		if err != nil {
			return false, err
		}
		var external map[string]any
		for _, manifest := range rendered.Manifests {
			if manifest["kind"] == "ExternalSecret" {
				external = manifest
			}
		}
		reference, err := json.Marshal(application.Env)
		if err != nil {
			return false, err
		}
		ready, err := client.RefreshEnvironmentConsumer(ctx, kubeapps.EnvironmentConsumerInput{Namespace: stored.Space.Namespace, AppName: application.Name, TargetID: stored.Space.ID, Revision: revision, Generation: postgresID + "/" + strconv.Itoa(current.Version), PreviousSecret: previousSecret, Reference: string(reference), ExternalSecret: external, ExpectedValues: current.Values})
		if err != nil {
			return false, err
		}
		complete = complete && ready
		stored.Space.Configuration.Apps[index] = application
		if stored.EnvSelectors == nil {
			stored.EnvSelectors = map[string]EnvReference{}
		}
		stored.EnvSelectors[application.Name] = application.Env
		found := false
		for i, previous := range stored.EnvSnapshots {
			if previous.AppName == application.Name {
				stored.EnvSnapshots[i], found = snapshot, true
			}
		}
		if !found {
			stored.EnvSnapshots = append(stored.EnvSnapshots, snapshot)
		}
		manifests := make([]map[string]any, 0, len(stored.Manifests)+1)
		for _, manifest := range stored.Manifests {
			if manifest["kind"] == "ExternalSecret" && manifestText(manifest, "metadata", "labels", "app.kubernetes.io/name") == application.Name {
				continue
			}
			if manifest["kind"] == "Deployment" && manifestText(manifest, "metadata", "name") == application.Name {
				metadata := manifest["metadata"].(map[string]any)
				annotations, _ := metadata["annotations"].(map[string]any)
				if annotations == nil {
					annotations = map[string]any{}
					metadata["annotations"] = annotations
				}
				annotations["borq.run/environment-group"] = string(reference)
				containers := manifest["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)
				containers[0].(map[string]any)["envFrom"] = []any{map[string]any{"secretRef": map[string]any{"name": EnvSecretName(snapshot)}}}
			}
			manifests = append(manifests, manifest)
		}
		stored.Manifests = append(manifests, external)
		changed = true
	}
	if !complete || !changed {
		return complete, nil
	}
	updated, err := json.Marshal(stored)
	if err != nil {
		return false, err
	}
	if bytes.Equal(original, updated) {
		return true, nil
	}
	write, err := documentWrite("space", key, stored, record.Revision)
	if err != nil {
		return false, err
	}
	return true, manager.database.Commit(ctx, []sqlite.Write{write})
}

func manifestText(manifest map[string]any, path ...string) string {
	var current any = manifest
	for _, key := range path {
		value, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current = value[key]
	}
	text, _ := current.(string)
	return text
}
