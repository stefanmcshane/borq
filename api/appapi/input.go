package appapi

import (
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
)

// NormalizeInput trims declared scalar settings before validation. Commands,
// credentials and arbitrary values retain their exact contents. The schema pointer
// follows kin-openapi's resolved schema representation.
func NormalizeInput(value any, schema *openapi3.Schema) {
	if schema == nil {
		return
	}
	switch fields := value.(type) {
	case map[string]any:
		for name, field := range fields {
			definition := schema.Properties[name]
			if definition == nil {
				definition = schema.AdditionalProperties.Schema
				if definition == nil || definition.Value == nil || len(definition.Value.Properties) == 0 {
					continue
				}
			}
			if definition.Value == nil {
				continue
			}
			if quantities, ok := field.(map[string]any); ok && (name == "requests" || name == "limits") && definition.Value.AdditionalProperties.Schema != nil && definition.Value.AdditionalProperties.Schema.Value.Type.Is("string") {
				for resource, quantity := range quantities {
					if text, ok := quantity.(string); ok {
						quantities[resource] = strings.TrimSpace(text)
					}
				}
			}
			if text, ok := field.(string); ok && trimInputField(name) && definition.Value.Type.Is("string") {
				fields[name] = strings.TrimSpace(text)
				continue
			}
			NormalizeInput(field, definition.Value)
		}
	case []any:
		if schema.Items == nil {
			return
		}
		for _, item := range fields {
			NormalizeInput(item, schema.Items.Value)
		}
	}
}

func trimInputPath(parts []string, schemas openapi3.Schemas) {
	for index := 1; index < len(parts); index++ {
		var schema *openapi3.Schema
		switch parts[index-1] {
		case "organizations", "clusters", "spaces", "plans", "operations", "postgres-operations", "custom-chart-operations":
			schema = schemas["Organization"].Value.Properties["name"].Value
		case "apps":
			schema = schemas["App"].Value.Properties["name"].Value
		case "custom-charts":
			schema = schemas["CustomChart"].Value.Properties["name"].Value
		case "postgres":
			schema = schemas["Postgres"].Value.Properties["name"].Value
		case "environment-groups":
			schema = schemas["CloneEnvironmentGroupInput"].Value.Properties["sourceName"].Value
		case "versions":
			schema = schemas["CloneEnvironmentGroupInput"].Value.Properties["sourceVersion"].Value
		default:
			continue
		}
		name := strings.TrimSpace(parts[index])
		if name != parts[index] && schema.VisitJSON(name) == nil {
			parts[index] = name
		}
	}
}

func trimInputField(name string) bool {
	switch name {
	case "name", "image", "host", "path", "organization", "space", "cluster",
		"context", "clientApiServer", "kubeconfigPath", "sourceName", "sourceVersion",
		"version", "chart", "repo", "repository", "region", "bucket", "prefix", "endpoint",
		"storage", "storageClass", "database", "username", "schedule", "rotationPeriod",
		"interval", "recipient", "credentialsRef", "backupId", "recoveryTarget", "jobUid",
		"id", "deployment", "namespace", "certificateIssuer", "profile", "planID", "targetID":
		return true
	}
	return false
}
