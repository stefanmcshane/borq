package apps

import (
	"crypto/sha256"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// EnvSecretName derives a stable Secret name for a pinned version or live database binding.
func EnvSecretName(snapshot EnvSnapshot) string {
	identity := sha256.Sum256([]byte(snapshot.SnapshotID))
	if snapshot.PostgresID != "" {
		return fmt.Sprintf("%s-env-%x-live", snapshot.AppName, identity[:6])
	}
	return fmt.Sprintf("%s-env-%x-v%d", snapshot.AppName, identity[:6], snapshot.Version)
}

func validateEnvReference(reference EnvReference) error {
	if reference.Name != "" && len(validation.IsDNS1123Subdomain(reference.Name)) > 0 {
		return fmt.Errorf("environment group name must be a DNS subdomain")
	}
	if reference.Version == "latest" {
		return nil
	}
	version, err := strconv.Atoi(reference.Version)
	if err != nil || version < 0 || strconv.Itoa(version) != reference.Version {
		return fmt.Errorf("environment group version must be latest or a nonnegative decimal integer")
	}
	return nil
}

func renderEnvSnapshots(configuration Configuration, targetID, _ string, snapshots []EnvSnapshot) ([]EnvSnapshot, error) {
	if snapshots == nil {
		snapshots = []EnvSnapshot{}
		for _, application := range configuration.Apps {
			if application.Env.Version == "latest" || application.Env.Version == "0" {
				continue
			}
			version, _ := strconv.Atoi(application.Env.Version)
			snapshots = append(snapshots, EnvSnapshot{
				AppName: application.Name, ConfigName: application.Env.Name, Store: "borq-openbao",
				Path: "borq/environment-groups/" + targetID + "/" + application.Env.Name, Version: version,
				SnapshotID: targetID + "/" + application.Env.Name + "/" + application.Env.Version,
				Values:     []Variable{},
			})
		}
	}
	byApp := make(map[string]EnvSnapshot, len(snapshots))
	for _, snapshot := range snapshots {
		if err := validateEnvSnapshot(snapshot, targetID); err != nil {
			return nil, err
		}
		if _, exists := byApp[snapshot.AppName]; exists {
			return nil, fmt.Errorf("app %q has duplicate environment snapshots", snapshot.AppName)
		}
		snapshot.Values = slices.Clone(snapshot.Values)
		slices.SortFunc(snapshot.Values, func(left, right Variable) int { return strings.Compare(left.Name, right.Name) })
		for index, variable := range snapshot.Values {
			if len(validation.IsEnvVarName(variable.Name)) > 0 || (index > 0 && snapshot.Values[index-1].Name == variable.Name) {
				return nil, fmt.Errorf("app %q environment has invalid or duplicate variable names", snapshot.AppName)
			}
		}
		byApp[snapshot.AppName] = snapshot
	}
	resolved := make([]EnvSnapshot, 0, len(snapshots))
	for _, application := range configuration.Apps {
		snapshot, exists := byApp[application.Name]
		if exists && snapshot.PostgresID != "" {
			if application.Env.Version != "latest" || snapshot.ConfigName != application.Env.Name {
				return nil, fmt.Errorf("app %q managed database environment must follow latest", application.Name)
			}
			resolved = append(resolved, snapshot)
			delete(byApp, application.Name)
			continue
		}
		if application.Env.Version == "latest" || application.Env.Version == "0" {
			if exists {
				return nil, fmt.Errorf("app %q has an environment snapshot without a positive pinned version", application.Name)
			}
			continue
		}
		if !exists || snapshot.ConfigName != application.Env.Name || strconv.Itoa(snapshot.Version) != application.Env.Version {
			return nil, fmt.Errorf("app %q requires a snapshot matching its environment group reference", application.Name)
		}
		resolved = append(resolved, snapshot)
		delete(byApp, application.Name)
	}
	if len(byApp) != 0 {
		return nil, fmt.Errorf("environment snapshot has no matching application")
	}
	return resolved, nil
}

func validateEnvSnapshot(snapshot EnvSnapshot, targetID string) error {
	if len(snapshot.AppName) > 50 || len(validation.IsDNS1035Label(snapshot.AppName)) > 0 {
		return fmt.Errorf("environment snapshot requires a valid application name")
	}
	if snapshot.ConfigName == "" || len(validation.IsDNS1123Subdomain(snapshot.ConfigName)) > 0 {
		return fmt.Errorf("app %q requires a valid environment group name", snapshot.AppName)
	}
	if len(validation.IsDNS1123Subdomain(snapshot.Store)) > 0 {
		return fmt.Errorf("app %q requires a valid environment store name", snapshot.AppName)
	}
	if snapshot.Path != environmentGroupPath(targetID, snapshot.ConfigName) && snapshot.Path != legacyEnvironmentGroupPath(targetID, snapshot.ConfigName) {
		return fmt.Errorf("app %q environment path must belong to its deployment target and config", snapshot.AppName)
	}
	if snapshot.PostgresID != "" {
		if snapshot.Version != 0 || snapshot.SnapshotID != targetID+"/"+snapshot.ConfigName+"/"+snapshot.PostgresID {
			return fmt.Errorf("app %q managed environment requires its producer identity without a pinned version", snapshot.AppName)
		}
		for _, variable := range snapshot.Values {
			if variable.Value != "" {
				return fmt.Errorf("app %q live environment must not contain credential values", snapshot.AppName)
			}
		}
		return nil
	}
	if snapshot.Version < 1 || strings.TrimSpace(snapshot.SnapshotID) == "" {
		return fmt.Errorf("app %q environment requires a positive version and snapshot identity", snapshot.AppName)
	}
	return nil
}
