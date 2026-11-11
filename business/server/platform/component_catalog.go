package platform

import "fmt"

// ComponentScope selects the installation whose desired configuration is shown.
type ComponentScope string

const (
	// ComponentScopeManagement contains services on Borq's hosting cluster.
	ComponentScopeManagement ComponentScope = "management"
	// ComponentScopeTarget contains services installed on registered deployment clusters.
	ComponentScopeTarget ComponentScope = "target"
	// ComponentScopePostgres contains controllers installed when PostgreSQL is requested.
	ComponentScopePostgres ComponentScope = "postgres"
)

// ComponentCatalogInput supplies saved settings and the preview installation context.
type ComponentCatalogInput struct {
	// Settings contains runtime defaults and accepted shared overrides.
	Settings Settings
	// Scope selects management, target, or PostgreSQL components.
	Scope ComponentScope
	// ClusterID is the registered target identity used in telemetry routes.
	ClusterID string
}

// ComponentCatalog describes the configuration Borq sends to Helm without contacting a cluster.
type ComponentCatalog struct {
	// Scope identifies the installation represented by the components.
	Scope ComponentScope `json:"scope"`
	// Profile selects Borq's resource and storage defaults.
	Profile string `json:"profile"`
	// Namespace is the default release namespace; components can use a shared namespace.
	Namespace string `json:"namespace"`
	// ClusterID identifies the registered target used to expand runtime values.
	ClusterID string `json:"clusterId,omitempty"`
	// Components contains the resolved catalog in installation order.
	Components []ComponentConfiguration `json:"components"`
}

// ComponentConfiguration separates effective Helm input from saved user overrides.
type ComponentConfiguration struct {
	// Name is Borq's stable component and Helm release name.
	Name string `json:"name"`
	// Namespace is the effective Helm release namespace, separate from chart values.
	Namespace string `json:"namespace"`
	// Description explains the component's role.
	Description string `json:"description"`
	// Repository is the effective upstream Helm repository, if the chart uses one.
	Repository string `json:"repository"`
	// Chart is the effective chart name or archive URL.
	Chart string `json:"chart"`
	// Version is the exact chart pin Borq requests.
	Version string `json:"version"`
	// Values combines Borq's rendered defaults with saved values overrides.
	Values map[string]any `json:"values"`
	// Override contains only saved customization, suitable for editing and export.
	Override ComponentOverride `json:"override"`
}

// GetComponentCatalog renders the same desired Helm input used during installation.
// It does not fetch upstream chart defaults, resolve credentials, or contact Kubernetes.
func GetComponentCatalog(input ComponentCatalogInput) (ComponentCatalog, error) {
	settings, err := normalizeSettings(input.Settings)
	if err != nil {
		return ComponentCatalog{}, err
	}
	var definitions []component
	switch input.Scope {
	case ComponentScopeManagement:
		definitions = managementCatalog(settings, false)
	case ComponentScopeTarget:
		settings, err = targetSettings(TargetInput{ID: input.ClusterID, Settings: settings})
		if err != nil {
			return ComponentCatalog{}, err
		}
		definitions = networkTargetCatalog(settings, false)
	case ComponentScopePostgres:
		settings.Namespace = SystemNamespace
		definitions = postgresComponents(settings)
	default:
		return ComponentCatalog{}, fmt.Errorf("unsupported component scope %q", input.Scope)
	}
	result := ComponentCatalog{Scope: input.Scope, Profile: settings.Profile, Namespace: settings.Namespace, Components: []ComponentConfiguration{}}
	if input.Scope == ComponentScopeTarget {
		result.ClusterID = input.ClusterID
	}
	for _, definition := range definitions {
		values, err := renderComponentValues(definition, valuesInput{Settings: settings, ClusterID: result.ClusterID})
		if err != nil {
			return ComponentCatalog{}, err
		}
		result.Components = append(result.Components, ComponentConfiguration{Name: definition.Name, Namespace: definition.namespace(settings.Namespace), Description: definition.Description, Repository: definition.Repository, Chart: definition.Chart, Version: definition.Version, Values: values, Override: settings.Components[definition.Name]})
	}
	return result, nil
}
