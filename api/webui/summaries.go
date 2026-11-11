package webui

import "github.com/stefanmcshane/borq/api/appapi"

type applicationSummary struct {
	// Name and Image identify the application in resource lists.
	Name  string `json:"name"`
	Image string `json:"image"`
	// Port and Replicas describe its deployment without commands or manifests.
	Port     int `json:"port"`
	Replicas int `json:"replicas"`
	// Env identifies the published environment version, without values.
	Env appapi.EnvReference `json:"env"`
}

type configurationSummary struct {
	// Apps contains only metadata needed by navigation and resource lists.
	Apps []applicationSummary `json:"apps"`
}

type spaceSummary struct {
	// ID, Name and Organization identify the deployment binding.
	ID           string `json:"id"`
	Name         string `json:"name"`
	Organization string `json:"organization"`
	// Cluster and Namespace locate the binding without credentials.
	Cluster   string `json:"cluster"`
	Namespace string `json:"namespace"`
	// Revision and LastSuccessfulRevision describe accepted and deployed state.
	Revision               int `json:"revision"`
	LastSuccessfulRevision int `json:"lastSuccessfulRevision"`
	// Configuration supplies application summaries without private configuration.
	Configuration configurationSummary `json:"configuration"`
}

type applicationListData struct {
	// Configuration supplies application summaries without saved manifests.
	Configuration configurationSummary `json:"configuration"`
}

type planData struct {
	// ID and BaseRevision identify the immutable artifacts awaiting apply.
	ID           string `json:"id"`
	BaseRevision int    `json:"baseRevision"`
	// Space identifies the target without its accepted configuration.
	Space spaceSummary `json:"space"`
	// ConfigurationDiff and Manifests are the exact artifacts shown for review.
	ConfigurationDiff string            `json:"configurationDiff"`
	Manifests         []appapi.Manifest `json:"manifests"`
	// ResourceDiffs contains only the identities and actions displayed in the table.
	ResourceDiffs []resourceDiffSummary `json:"resourceDiffs"`
	configuration appapi.Configuration
}

type resourceDiffSummary struct {
	// Kind, Name and Action identify the change without duplicating resource bodies.
	Kind   string                    `json:"kind"`
	Name   string                    `json:"name"`
	Action appapi.ResourceDiffAction `json:"action"`
}

func summarizeConfiguration(configuration appapi.Configuration) configurationSummary {
	var summary configurationSummary
	for _, application := range configuration.Apps {
		summary.Apps = append(summary.Apps, applicationSummary{
			Name: application.Name, Image: application.Image, Port: application.Port,
			Replicas: application.Replicas, Env: application.Env,
		})
	}
	return summary
}

func summarizeSpace(space appapi.Space) spaceSummary {
	return spaceSummary{
		ID: space.ID, Name: space.Name, Organization: space.Organization,
		Cluster: space.Cluster, Namespace: space.Namespace, Revision: space.Revision,
		LastSuccessfulRevision: space.LastSuccessfulRevision,
		Configuration:          summarizeConfiguration(space.Configuration),
	}
}

func summarizePlan(plan appapi.Plan) planData {
	summary := planData{
		ID: plan.ID, BaseRevision: plan.BaseRevision, Space: summarizeSpace(plan.Space),
		ConfigurationDiff: plan.ConfigurationDiff, Manifests: plan.Manifests,
		configuration: plan.Configuration,
	}
	for _, resource := range plan.ResourceDiffs {
		summary.ResourceDiffs = append(summary.ResourceDiffs, resourceDiffSummary{
			Kind: resource.Kind, Name: resource.Name, Action: resource.Action,
		})
	}
	return summary
}
