package apps

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

type inspectedEnvironment struct {
	snapshot EnvSnapshot
	keys     []string
}

// EnvReferences extracts pinned vault bindings without exposing environment
// values, allowing the server to resolve exactly the versions in manifests.
func EnvReferences(manifests []map[string]any) ([]EnvSnapshot, error) {
	resources, err := inspectResources(manifests)
	if err != nil {
		return nil, err
	}
	environments, err := inspectEnvironments(resources, nil)
	if err != nil {
		return nil, err
	}
	references := make([]EnvSnapshot, 0, len(environments))
	for _, environment := range environments {
		references = append(references, environment.snapshot)
	}
	slices.SortFunc(references, func(left, right EnvSnapshot) int { return strings.Compare(left.AppName, right.AppName) })
	return references, nil
}

func inspectEnvironments(resources []inspectedResource, snapshots []EnvSnapshot) (map[string]inspectedEnvironment, error) {
	environments := make(map[string]inspectedEnvironment)
	for _, resource := range resources {
		if resource.kind != "ExternalSecret" {
			continue
		}
		if _, exists := environments[resource.app]; exists {
			return nil, fmt.Errorf("app %q has duplicate environment ExternalSecrets", resource.app)
		}
		environment, err := inspectExternalSecret(resource)
		if err != nil {
			return nil, fmt.Errorf("ExternalSecret/%s: %w", resource.name, err)
		}
		environments[resource.app] = environment
	}
	// Readback uses public config identities; raw values are deliberately unnecessary.
	for _, snapshot := range snapshots {
		environment, exists := environments[snapshot.AppName]
		if !exists || snapshot.ConfigName != environment.snapshot.ConfigName ||
			snapshot.Store != environment.snapshot.Store || snapshot.Path != environment.snapshot.Path ||
			snapshot.Version != environment.snapshot.Version || snapshot.PostgresID != environment.snapshot.PostgresID || snapshot.SnapshotID != environment.snapshot.SnapshotID {
			return nil, fmt.Errorf("app %q snapshot does not match its pinned ExternalSecret binding", snapshot.AppName)
		}
	}
	return environments, nil
}

func inspectExternalSecret(resource inspectedResource) (inspectedEnvironment, error) {
	secret := struct {
		metav1.ObjectMeta `json:"metadata"`
		Spec              struct {
			RefreshPolicy  string `json:"refreshPolicy"`
			SecretStoreRef struct {
				Kind string `json:"kind"`
				Name string `json:"name"`
			} `json:"secretStoreRef"`
			Data     []any `json:"data"`
			DataFrom []struct {
				Extract struct {
					Key                string `json:"key"`
					Version            string `json:"version"`
					Property           string `json:"property"`
					ConversionStrategy string `json:"conversionStrategy"`
					DecodingStrategy   string `json:"decodingStrategy"`
					MetadataPolicy     string `json:"metadataPolicy"`
					NullBytePolicy     string `json:"nullBytePolicy"`
				} `json:"extract"`
			} `json:"dataFrom"`
			Target struct {
				Name           string `json:"name"`
				CreationPolicy string `json:"creationPolicy"`
				DeletionPolicy string `json:"deletionPolicy"`
				Immutable      bool   `json:"immutable"`
				Template       struct {
					Type          string            `json:"type"`
					Metadata      metav1.ObjectMeta `json:"metadata"`
					EngineVersion string            `json:"engineVersion"`
					MergePolicy   string            `json:"mergePolicy"`
					Data          map[string]string `json:"data"`
					TemplateFrom  []any             `json:"templateFrom"`
				} `json:"template"`
			} `json:"target"`
		} `json:"spec"`
	}{}
	if err := decodeManifest(resource.manifest, &secret); err != nil {
		return inspectedEnvironment{}, err
	}
	if secret.Labels["borq.run/phase"] != "environment" || (secret.Spec.RefreshPolicy != "OnChange" && secret.Annotations["borq.run/postgres-id"] == "") ||
		secret.Spec.SecretStoreRef.Kind != "ClusterSecretStore" || len(secret.Spec.Data) != 0 || len(secret.Spec.DataFrom) != 1 {
		return inspectedEnvironment{}, fmt.Errorf("environment must use one versioned ClusterSecretStore extract with OnChange refresh")
	}
	extract := secret.Spec.DataFrom[0].Extract
	version, err := strconv.Atoi(extract.Version)
	producer := secret.Annotations["borq.run/postgres-id"]
	if producer != "" && extract.Version == "" {
		version, err = 0, nil
	}
	if err != nil || (producer == "" && extract.Version != strconv.Itoa(version)) || extract.Property != "values" ||
		(extract.ConversionStrategy != "" && extract.ConversionStrategy != "Default") ||
		(extract.DecodingStrategy != "" && extract.DecodingStrategy != "None") ||
		(extract.MetadataPolicy != "" && extract.MetadataPolicy != "None") ||
		(extract.NullBytePolicy != "" && extract.NullBytePolicy != "Ignore") {
		return inspectedEnvironment{}, fmt.Errorf("environment extract must select a positive version of the values property without transformations")
	}
	snapshot := EnvSnapshot{
		AppName: resource.app, ConfigName: cmp.Or(secret.Annotations["borq.run/environment-group-name"], secret.Annotations["borq.run/env-config-name"]), Store: secret.Spec.SecretStoreRef.Name,
		Path: extract.Key, Version: version, PostgresID: producer, SnapshotID: secret.Annotations["borq.run/env-snapshot"],
	}
	if err := validateEnvSnapshot(snapshot, resource.target); err != nil {
		return inspectedEnvironment{}, err
	}
	target := secret.Spec.Target
	template := target.Template
	if resource.name != EnvSecretName(snapshot) || target.Name != resource.name ||
		target.CreationPolicy != "Owner" || target.DeletionPolicy != "Merge" || target.Immutable != (producer == "") ||
		template.Type != "Opaque" || (template.EngineVersion != "" && template.EngineVersion != "v2") ||
		(template.MergePolicy != "" && template.MergePolicy != "Replace") || len(template.Data) != 0 || len(template.TemplateFrom) != 0 {
		return inspectedEnvironment{}, fmt.Errorf("environment must create the owned immutable Opaque Secret for its snapshot")
	}
	if template.Metadata.Labels["app.kubernetes.io/name"] != resource.app ||
		template.Metadata.Labels["app.kubernetes.io/managed-by"] != "borq" ||
		template.Metadata.Labels["borq.run/target-id"] != resource.target ||
		template.Metadata.Annotations["borq.run/env-path"] != snapshot.Path ||
		cmp.Or(template.Metadata.Annotations["borq.run/environment-group-name"], template.Metadata.Annotations["borq.run/env-config-name"]) != snapshot.ConfigName ||
		template.Metadata.Annotations["borq.run/env-version"] != cmp.Or(extract.Version, "latest") ||
		template.Metadata.Annotations["borq.run/postgres-id"] != producer ||
		(producer != "" && (secret.Spec.RefreshPolicy != "Periodic" || extract.Version != "")) {
		return inspectedEnvironment{}, fmt.Errorf("environment Secret template must identify its application, target, path and version")
	}
	keys := []string{}
	if err := json.Unmarshal([]byte(template.Metadata.Annotations["borq.run/env-keys"]), &keys); err != nil || keys == nil || !slices.IsSorted(keys) {
		return inspectedEnvironment{}, fmt.Errorf("environment Secret template requires sorted environment keys")
	}
	for index, key := range keys {
		if len(validation.IsEnvVarName(key)) != 0 || (index > 0 && keys[index-1] == key) {
			return inspectedEnvironment{}, fmt.Errorf("environment Secret template contains invalid or duplicate environment keys")
		}
	}
	if err := inspectExternalSecretFields(resource.manifest); err != nil {
		return inspectedEnvironment{}, err
	}
	return inspectedEnvironment{snapshot: snapshot, keys: keys}, nil
}

func inspectExternalSecretFields(manifest map[string]any) error {
	// Reject extra behavior while allowing the defaults added by the operator's
	// CRD. Controller status and top-level metadata remain outside the model.
	normalized := map[string]any{}
	if err := decodeManifest(manifest, &normalized); err != nil {
		return err
	}
	spec := normalized["spec"].(map[string]any)
	type allowedFields struct {
		object map[string]any
		keys   []string
	}
	checks := []allowedFields{
		{spec, []string{"refreshInterval", "refreshPolicy", "secretStoreRef", "target", "data", "dataFrom"}},
		{spec["secretStoreRef"].(map[string]any), []string{"name", "kind"}},
	}
	dataFrom := spec["dataFrom"].([]any)[0].(map[string]any)
	target := spec["target"].(map[string]any)
	template := target["template"].(map[string]any)
	checks = append(checks,
		allowedFields{dataFrom, []string{"extract"}},
		allowedFields{dataFrom["extract"].(map[string]any), []string{"key", "version", "property", "conversionStrategy", "decodingStrategy", "metadataPolicy", "nullBytePolicy"}},
		allowedFields{target, []string{"name", "creationPolicy", "deletionPolicy", "immutable", "template"}},
		allowedFields{template, []string{"type", "metadata", "engineVersion", "mergePolicy", "data", "templateFrom"}},
		allowedFields{template["metadata"].(map[string]any), []string{"labels", "annotations"}},
	)
	for _, check := range checks {
		for key := range check.object {
			if !slices.Contains(check.keys, key) {
				return fmt.Errorf("environment ExternalSecret contains unsupported field %q", key)
			}
		}
	}
	return nil
}

func inspectEnvironmentReference(variables []corev1.EnvVar, sources []corev1.EnvFromSource, environment inspectedEnvironment, annotation string) (EnvReference, error) {
	if len(variables) != 0 {
		return EnvReference{}, fmt.Errorf("inline environment variables are not supported; select a named environment group")
	}
	reference := EnvReference{}
	decoder := json.NewDecoder(strings.NewReader(annotation))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&reference); err != nil {
		return EnvReference{}, fmt.Errorf("deployment requires its environment group reference annotation")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return EnvReference{}, fmt.Errorf("environment group annotation must contain one object")
	}
	if err := validateEnvReference(reference); err != nil {
		return EnvReference{}, err
	}
	if reference.Version == "latest" && environment.snapshot.PostgresID != "" {
		if reference.Name != environment.snapshot.ConfigName || len(sources) != 1 {
			return EnvReference{}, fmt.Errorf("managed environment must match one live ExternalSecret")
		}
	} else if reference.Version == "latest" || reference.Version == "0" {
		if len(sources) != 0 || environment.snapshot.AppName != "" {
			return EnvReference{}, fmt.Errorf("unbound environment group cannot reference a Secret")
		}
		return reference, nil
	}
	if environment.snapshot.ConfigName != reference.Name || (environment.snapshot.PostgresID == "" && strconv.Itoa(environment.snapshot.Version) != reference.Version) || len(sources) != 1 {
		return EnvReference{}, fmt.Errorf("environment group must match one pinned ExternalSecret")
	}
	source := sources[0]
	if source.Prefix != "" || source.ConfigMapRef != nil || source.SecretRef == nil ||
		source.SecretRef.Name != EnvSecretName(environment.snapshot) || (source.SecretRef.Optional != nil && *source.SecretRef.Optional) {
		return EnvReference{}, fmt.Errorf("environment must use its required immutable snapshot Secret without transformations")
	}
	return reference, nil
}
