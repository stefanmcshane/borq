package platform

// TargetSetup describes required deployment infrastructure before a cluster is linked.
type TargetSetup struct {
	// Namespace is the destination for namespaced component resources on the linked cluster.
	Namespace string `json:"namespace"`
	// Components lists the pinned releases in their installation order.
	Components []SetupComponent `json:"components"`
}

// SetupComponent explains a required release without exposing private Helm values.
type SetupComponent struct {
	// Name is the Helm release name used during installation.
	Name string `json:"name"`
	// Version is the pinned chart version used for new installations.
	Version string `json:"version"`
	// Description explains the capability provided by the component.
	Description string `json:"description"`
}

// GetTargetSetup reads the same catalog and namespace used by target reconciliation.
func GetTargetSetup(settings Settings) (TargetSetup, error) {
	settings, err := normalizeSettings(settings)
	if err != nil {
		return TargetSetup{}, err
	}
	setup := TargetSetup{Namespace: settings.Namespace, Components: []SetupComponent{}}
	for _, definition := range overrideComponents(targetCatalog(false), settings.Components) {
		setup.Components = append(setup.Components, SetupComponent{Name: definition.Name, Version: definition.Version, Description: definition.Description})
	}
	return setup, nil
}
