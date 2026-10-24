package apps

import (
	"embed"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"slices"
	"strings"

	"github.com/stefanmcshane/borq/business/server/platform"
	chartcommon "helm.sh/helm/v4/pkg/chart/common"
	chartutil "helm.sh/helm/v4/pkg/chart/common/util"
	"helm.sh/helm/v4/pkg/chart/loader/archive"
	"helm.sh/helm/v4/pkg/chart/v2/loader"
	"helm.sh/helm/v4/pkg/engine"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/validation"
	yamlreader "k8s.io/apimachinery/pkg/util/yaml"
)

// chartFiles must be package scoped because go:embed attaches compiled runtime
// assets to a variable; embedding makes saved renderings independent of cwd.
//
//go:embed charts/app
var chartFiles embed.FS

// ResourceScope identifies the organization, space, and cluster outside apps.yaml.
type ResourceScope struct {
	// Organization is the organization's unique name, which is its current identifier.
	Organization string `json:"organization"`
	// Space is the space's readable name within the organization.
	Space string `json:"space"`
	// Cluster is the registered cluster's readable name within the organization.
	Cluster string `json:"cluster"`
}

type resourceMetadata struct {
	// Labels stores the ownership fields frozen in rendering values.
	Labels map[string]string `json:"labels"`
	// Annotations mirrors the identifiers without consuming selector semantics.
	Annotations map[string]string `json:"annotations"`
}

// Render expands the embedded chart without installing a Helm release. It
// records the exact values and manifests needed to apply an immutable plan.
func Render(configuration Configuration, namespace string, targetID string) (Rendered, error) {
	return RenderForPlan(configuration, namespace, targetID, "preview", "borq")
}

// RenderForPlan freezes application resources and predeploy Jobs for one exact
// saved plan. The plan ID makes each execution's Jobs immutable and resumable.
func RenderForPlan(configuration Configuration, namespace string, targetID string, planID string, certificateIssuer string) (Rendered, error) {
	return RenderWithEnv(configuration, namespace, targetID, planID, certificateIssuer, nil, ResourceScope{})
}

// RenderWithEnv renders immutable vault references supplied by the server. A nil
// snapshot list creates preview bindings without writing to a vault or cluster.
func RenderWithEnv(configuration Configuration, namespace, targetID, planID, certificateIssuer string, snapshots []EnvSnapshot, scope ResourceScope) (Rendered, error) {
	return renderWithEnv(configuration, namespace, targetID, planID, certificateIssuer, snapshots, scope, nil)
}

// A nil task selection renders all configured tasks; an explicit list limits
// execution without discarding the other applications' desired task settings.
func renderWithEnv(configuration Configuration, namespace, targetID, planID, certificateIssuer string, snapshots []EnvSnapshot, scope ResourceScope, predeployApps []string) (Rendered, error) {
	if problems := validation.IsDNS1123Label(namespace); len(problems) > 0 {
		return Rendered{}, fmt.Errorf("invalid namespace %q: %s", namespace, strings.Join(problems, "; "))
	}
	if problems := validation.IsValidLabelValue(targetID); targetID == "" || len(problems) > 0 {
		return Rendered{}, fmt.Errorf("target ID must be a nonempty Kubernetes label value")
	}
	if planID == "" {
		return Rendered{}, fmt.Errorf("plan ID is required")
	}
	certificateIssuer, err := normalizeCertificateIssuer(certificateIssuer)
	if err != nil {
		return Rendered{}, err
	}
	canonical, err := Merge(configuration, Patch{})
	if err != nil {
		return Rendered{}, err
	}
	snapshots, err = renderEnvSnapshots(canonical, targetID, planID, snapshots)
	if err != nil {
		return Rendered{}, err
	}
	identity := map[string]string{"borq.run/space-id": targetID}
	if scope != (ResourceScope{}) {
		if err := validateNames(scope.Organization, scope.Space, scope.Cluster); err != nil {
			return Rendered{}, fmt.Errorf("invalid render scope: %w", err)
		}
		identity["borq.run/organization-id"] = scope.Organization
		identity["borq.run/organization"] = scope.Organization
		identity["borq.run/space"] = scope.Space
		identity["borq.run/cluster-id"] = platform.ClusterID(scope.Organization, scope.Cluster)
		identity["borq.run/cluster"] = scope.Cluster
	}
	values, err := renderValues(canonical, namespace, targetID, snapshots, identity)
	if err != nil {
		return Rendered{}, err
	}
	values["certificateIssuer"] = certificateIssuer
	for index, value := range values["apps"].([]any) {
		application := value.(map[string]any)
		selected := predeployApps == nil || slices.Contains(predeployApps, canonical.Apps[index].Name)
		application["runPredeploy"] = selected
		if !selected && canonical.Apps[index].Predeploy.Command != "" {
			application["retainedPredeploy"] = canonical.Apps[index].Predeploy
		}
		if selected && canonical.Apps[index].Predeploy.Command != "" {
			values["planID"] = planID
		}
	}
	files, err := embeddedChartFiles()
	if err != nil {
		return Rendered{}, err
	}
	chart, err := loader.LoadFiles(files)
	if err != nil {
		return Rendered{}, fmt.Errorf("load embedded application chart: %w", err)
	}
	renderValues, err := chartutil.ToRenderValues(chart, values, chartcommon.ReleaseOptions{
		Name: "borq", Namespace: namespace, Revision: 1, IsInstall: true,
	}, nil)
	if err != nil {
		return Rendered{}, fmt.Errorf("prepare application chart values: %w", err)
	}
	templates, err := engine.Render(chart, renderValues)
	if err != nil {
		return Rendered{}, fmt.Errorf("render application chart: %w", err)
	}
	manifests, err := decodeManifests(templates)
	if err != nil {
		return Rendered{}, err
	}
	return Rendered{Values: values, Manifests: manifests, RendererVersion: chart.Metadata.Version, EnvSnapshots: snapshots}, nil
}

func renderValues(configuration Configuration, namespace, targetID string, snapshots []EnvSnapshot, identity map[string]string) (map[string]any, error) {
	encoded, err := json.Marshal(configuration)
	if err != nil {
		return nil, fmt.Errorf("encode complete application values: %w", err)
	}
	values := map[string]any{}
	if err := json.Unmarshal(encoded, &values); err != nil {
		return nil, fmt.Errorf("decode application values: %w", err)
	}
	values["namespace"] = namespace
	values["targetID"] = targetID
	values["resourceMetadata"] = renderMetadata(identity)
	bindings := make(map[string]EnvSnapshot, len(snapshots))
	for _, snapshot := range snapshots {
		bindings[snapshot.AppName] = snapshot
	}
	for index, entry := range values["apps"].([]any) {
		application := entry.(map[string]any)
		application["revision"] = applicationRevision(configuration.Apps[index])
		metadata := maps.Clone(identity)
		metadata["borq.run/app-name"] = application["name"].(string)
		metadata["borq.run/app-id"] = stableId(targetID + "/" + application["name"].(string))
		application["resourceMetadata"] = renderMetadata(metadata)
		application["envFrom"] = []any{}
		if snapshot, exists := bindings[application["name"].(string)]; exists {
			secret := EnvSecretName(snapshot)
			keys := make([]string, 0, len(snapshot.Values))
			for _, variable := range snapshot.Values {
				keys = append(keys, variable.Name)
			}
			application["envFrom"] = []any{map[string]any{"secretRef": map[string]any{"name": secret}}}
			application["envSnapshot"] = map[string]any{
				"store": snapshot.Store, "path": snapshot.Path, "version": snapshot.Version,
				"snapshotId": snapshot.SnapshotID, "secretName": secret, "keys": keys, "configName": snapshot.ConfigName,
			}
		}
		for _, entry := range application["ingresses"].([]any) {
			route := entry.(map[string]any)
			route["tlsSecret"] = ingressSecretName(application["name"].(string) + "-" + route["name"].(string))
		}
		command, err := renderCommand(application["command"].(string))
		if err != nil {
			return nil, err
		}
		application["command"] = command
		predeploy := application["predeploy"].(map[string]any)
		command, err = renderCommand(predeploy["command"].(string))
		if err != nil {
			return nil, err
		}
		predeploy["command"] = command
	}
	return values, nil
}

func renderMetadata(identity map[string]string) map[string]any {
	labels := make(map[string]any, len(identity))
	for key, value := range identity {
		labels[key] = value
	}
	return map[string]any{"labels": labels, "annotations": maps.Clone(labels)}
}

func embeddedChartFiles() ([]*archive.BufferedFile, error) {
	// Helm requires pointers for its archive file API; these do not escape into
	// Borq's domain models or deployment artifacts.
	files := []*archive.BufferedFile{}
	err := fs.WalkDir(chartFiles, "charts/app", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		contents, err := chartFiles.ReadFile(path)
		if err != nil {
			return err
		}
		files = append(files, &archive.BufferedFile{Name: strings.TrimPrefix(path, "charts/app/"), Data: contents})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read embedded application chart: %w", err)
	}
	return files, nil
}

func decodeManifests(templates map[string]string) ([]map[string]any, error) {
	paths := make([]string, 0, len(templates))
	for path := range templates {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	manifests := []map[string]any{}
	for _, path := range paths {
		decoder := yamlreader.NewYAMLOrJSONDecoder(strings.NewReader(templates[path]), 4096)
		for {
			resource := unstructured.Unstructured{}
			err := decoder.Decode(&resource)
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("decode application template %q: %w", path, err)
			}
			if len(resource.Object) == 0 {
				continue
			}
			manifests = append(manifests, resource.Object)
		}
	}
	return manifests, nil
}
