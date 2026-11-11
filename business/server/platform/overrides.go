package platform

import (
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
)

// ComponentOverride replaces a component's chart coordinates and merges inline values.
type ComponentOverride struct {
	// Repo is the public chart repository URL.
	Repo string `json:"repo,omitempty"`
	// Chart replaces the upstream chart name.
	Chart string `json:"chart,omitempty"`
	// Version pins an exact chart version.
	Version string `json:"version,omitempty"`
	// Values merges recursively over Borq's component defaults.
	Values map[string]any `json:"values,omitempty"`
}

// ValidateComponentOverrides rejects unknown components and malformed chart coordinates.
func ValidateComponentOverrides(overrides map[string]ComponentOverride) error {
	for name, override := range overrides {
		switch name {
		case "openbao", "external-secrets", "prometheus", "loki", "rustfs", "cert-manager", "traefik", "alloy", "snapshot-controller", "cloudnative-pg", "plugin-barman-cloud":
		default:
			return fmt.Errorf("unknown component %q", name)
		}
		if override.Repo != "" {
			location, err := url.Parse(override.Repo)
			if err != nil || location.Host == "" || location.User != nil || location.RawQuery != "" || location.Fragment != "" || (location.Scheme != "https" && location.Scheme != "http" && location.Scheme != "oci") {
				return fmt.Errorf("component %s repository must be a public HTTP(S) or OCI URL", name)
			}
		}
		if override.Chart != "" && !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`).MatchString(override.Chart) {
			return fmt.Errorf("component %s chart name is invalid", name)
		}
		if override.Version != "" && !regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`).MatchString(override.Version) {
			return fmt.Errorf("component %s version must be an exact chart pin", name)
		}
	}
	return nil
}

func overrideComponents(definitions []component, overrides map[string]ComponentOverride) []component {
	for index := range definitions {
		override := overrides[definitions[index].Name]
		if override.Repo != "" {
			definitions[index].Repository = override.Repo
		}
		if override.Chart != "" {
			definitions[index].Chart = override.Chart
		}
		if override.Version != "" {
			definitions[index].Version = override.Version
		}
	}
	return definitions
}

func mergeComponentValues(base, override map[string]any) map[string]any {
	result := maps.Clone(base)
	for key, value := range override {
		nested, ok := value.(map[string]any)
		previous, wasMap := result[key].(map[string]any)
		if ok && wasMap {
			result[key] = mergeComponentValues(previous, nested)
			continue
		}
		result[key] = value
	}
	return result
}

func componentSelected(names []string, name string) bool {
	return names == nil || slices.Contains(names, name)
}
