package platform

import (
	"context"
	"fmt"
	"strings"

	"github.com/pmezard/go-difflib/difflib"
	"github.com/stefanmcshane/borq/foundations/helm"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"sigs.k8s.io/yaml"
)

// ComponentDiff describes the target work observed before any installation writes.
type ComponentDiff struct {
	// Components follow catalog order; independent installations may overlap.
	Components []ComponentChange `json:"components"`
}

// ComponentChange compares one stored Helm release with Borq's pinned catalog.
type ComponentChange struct {
	// Name identifies the managed component and its Helm release.
	Name string `json:"name"`
	// Chart identifies the desired upstream chart.
	Chart string `json:"chart"`
	// InstalledVersion is empty when the release has not been installed.
	InstalledVersion string `json:"installedVersion"`
	// DesiredVersion is the pinned version in the current Borq catalog.
	DesiredVersion string `json:"desiredVersion"`
	// Revision is the current Helm revision, or zero for a missing release.
	Revision int `json:"revision"`
	// Action is install, repair, upgrade, none or conflict for this request.
	Action string `json:"action"`
	// Reason explains an existing installation that prevents safe reconciliation.
	Reason string `json:"reason,omitempty"`
	// UpgradeAvailable reports catalog changes even when Upgrade was not requested.
	UpgradeAvailable bool `json:"upgradeAvailable"`
	// ValuesDiff is a unified YAML diff with sensitive existing values redacted.
	ValuesDiff string `json:"valuesDiff"`
}

// DiffTarget checks physical ownership first, then reads Helm records to compare
// user values and chart pins. It never claims ownership, writes credentials,
// creates a namespace, downloads charts, or changes installed releases.
func DiffTarget(ctx context.Context, input TargetInput) (ComponentDiff, error) {
	ctx, span := otel.Tracer("borq/platform").Start(ctx, "platform.diff_target", trace.WithAttributes(attribute.String("cluster_id", input.ID), attribute.Bool("upgrade", input.Upgrade)))
	defer span.End()
	if err := CheckTargetOwnership(ctx, input); err != nil {
		return ComponentDiff{}, err
	}
	if err := ValidateServerSettings(input.Settings); err != nil {
		return ComponentDiff{}, err
	}
	settings, err := targetSettings(input)
	if err != nil {
		return ComponentDiff{}, err
	}
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: input.KubeconfigPath, Context: input.Context, InCluster: input.InCluster}, logger.Logger{})
	if err != nil {
		return ComponentDiff{}, err
	}
	if settings.Network.Tailscale != nil {
		if err := checkNetworkControllerConflicts(ctx, connection, settings, false); err != nil {
			return ComponentDiff{}, err
		}
	}
	releases, err := helm.NewReleaseManager(helm.NewReleaseManagerInput{Connection: connection, KubeconfigPath: input.KubeconfigPath})
	if err != nil {
		return ComponentDiff{}, err
	}
	defer releases.Close()
	conflicts, err := targetComponentConflicts(ctx, connection, releases, settings.Namespace, networkTargetCatalog(settings, input.Upgrade))
	if err != nil {
		return ComponentDiff{}, err
	}
	result := ComponentDiff{Components: []ComponentChange{}}
	for _, definition := range networkTargetCatalog(settings, input.Upgrade) {
		renderInput := valuesInput{Settings: settings, ClusterID: input.ID}
		if definition.Name == "tailscale-operator" {
			tag, err := networkOperatorTag(ctx, connection, renderInput)
			if err != nil {
				return result, err
			}
			renderInput.NetworkOperatorTag = tag
		}
		values, err := renderComponentValues(definition, renderInput)
		if err != nil {
			return result, err
		}
		inspection, err := releases.InspectWithValues(ctx, helm.InstallOrUpgradeInput{Namespace: definition.namespace(settings.Namespace), ReleaseName: definition.releaseName(), ChartRef: definition.Chart, RepositoryUrl: definition.Repository, Version: definition.Version, Values: values, PodLabels: componentPodLabels(), SharedCrds: definition.Name == "external-secrets" || definition.Name == "traefik"})
		if err != nil {
			return result, err
		}
		change, err := componentChange(definition, inspection, values, input.Upgrade && componentSelected(input.UpgradeComponents, definition.Name))
		if err != nil {
			return result, err
		}
		if input.Upgrade && !componentSelected(input.UpgradeComponents, definition.Name) {
			change.Action = "none"
		}
		if reason := conflicts[definition.Name]; reason != "" {
			change.Action = "conflict"
			change.Reason = reason
		}
		result.Components = append(result.Components, change)
	}
	return result, nil
}

func componentChange(definition component, inspection helm.ReleaseInspection, desired map[string]any, upgrade bool) (ComponentChange, error) {
	change := ComponentChange{Name: definition.Name, Chart: definition.Chart, InstalledVersion: inspection.State.Version, DesiredVersion: definition.Version, Revision: inspection.State.Revision, Action: "none", UpgradeAvailable: inspection.DefinitionChanged}
	var networkErr error
	if !upgrade {
		upgrade, networkErr = networkValuesUpgrade(definition, inspection, desired)
	}
	switch {
	case networkErr != nil:
		change.Action, change.Reason = "conflict", networkErr.Error()
	case inspection.State.Status == "absent":
		change.Action = "install"
	case inspection.State.Status != "deployed":
		change.Action = "repair"
	case inspection.DefinitionChanged && upgrade:
		change.Action = "upgrade"
	}
	diff, err := componentValuesDiff(inspection.Values, desired)
	if err != nil {
		return ComponentChange{}, fmt.Errorf("diff %s values: %w", definition.Name, err)
	}
	change.ValuesDiff = diff
	return change, nil
}

func componentValuesDiff(installed, desired map[string]any) (string, error) {
	// Only the desired catalog shape has known non-secret semantics. Preserve
	// unknown keys for review, while hiding their user-provided leaf values.
	publicInstalled := redactInstalledValues(installed, desired, true)
	before, err := yaml.Marshal(publicInstalled)
	if err != nil {
		return "", err
	}
	after, err := yaml.Marshal(desired)
	if err != nil {
		return "", err
	}
	if string(before) == string(after) {
		return "", nil
	}
	return difflib.GetUnifiedDiffString(difflib.UnifiedDiff{A: difflib.SplitLines(string(before)), B: difflib.SplitLines(string(after)), FromFile: "installed-values.yaml", ToFile: "desired-values.yaml", Context: 3})
}

func redactInstalledValues(installed, desired any, known bool) any {
	switch value := installed.(type) {
	case map[string]any:
		template, _ := desired.(map[string]any)
		result := make(map[string]any, len(value))
		for key, field := range value {
			expected, exists := template[key]
			// A pod volume's secret object contains a reference, not Secret data.
			// Recurse only through the known catalog shape; extra fields stay redacted.
			reference, _ := expected.(map[string]any)
			_, secretReference := reference["secretName"]
			if sensitiveValueKey(key) && !(known && exists && key == "secret" && secretReference) {
				result[key] = "[redacted]"
				continue
			}
			if key == "content" && exists {
				if source, ok := field.(string); ok && source != expected {
					result[key] = "[redacted existing configuration]"
					continue
				}
			}
			result[key] = redactInstalledValues(field, expected, known && exists)
		}
		return result
	case []any:
		template, _ := desired.([]any)
		result := make([]any, len(value))
		for index, field := range value {
			var expected any
			exists := index < len(template)
			if exists {
				expected = template[index]
			}
			result[index] = redactInstalledValues(field, expected, known && exists)
		}
		return result
	default:
		if !known {
			return "[redacted]"
		}
		return value
	}
}

func sensitiveValueKey(key string) bool {
	key = strings.ToLower(strings.NewReplacer("_", "", "-", "", ".", "").Replace(key))
	for _, word := range []string{"password", "token", "secret", "privatekey", "accesskey", "apikey", "authorization", "credential"} {
		if strings.Contains(key, word) {
			// Reference objects and names identify Kubernetes Secrets; their
			// contents are never part of the target chart values.
			return !strings.HasSuffix(key, "ref") && !strings.HasSuffix(key, "name") && key != "existingsecret"
		}
	}
	return false
}
