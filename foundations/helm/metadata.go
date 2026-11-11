package helm

import (
	"bytes"
	"fmt"
	"io"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	yamlreader "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

// Helm's post-renderer interface requires buffers; decoding stays at this boundary.
type metadataRenderer struct{ input InstallOrUpgradeInput }

func (renderer metadataRenderer) Run(source *bytes.Buffer) (*bytes.Buffer, error) {
	decoder := yamlreader.NewYAMLOrJSONDecoder(source, 4096)
	var output bytes.Buffer
	for {
		var document map[string]any
		if err := decoder.Decode(&document); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("decode Helm resource metadata: %w", err)
		}
		if len(document) == 0 {
			continue
		}
		if renderer.input.SharedCrds && document["apiVersion"] == "apiextensions.k8s.io/v1" && document["kind"] == "CustomResourceDefinition" {
			continue
		}
		if err := renderer.labelResource(document); err != nil {
			return nil, err
		}
		encoded, err := yaml.Marshal(document)
		if err != nil {
			return nil, err
		}
		output.WriteString("---\n")
		output.Write(encoded)
	}
	return &output, nil // Helm's PostRenderer interface requires a buffer pointer.
}

func (renderer metadataRenderer) labelResource(document map[string]any) error {
	object := unstructured.Unstructured{Object: document}
	if object.IsList() {
		return object.EachListItem(func(item runtime.Object) error {
			resource, ok := item.(*unstructured.Unstructured)
			if !ok {
				return fmt.Errorf("unsupported Helm list item %T", item)
			}
			return renderer.labelResource(resource.Object)
		})
	}
	if err := mergeResourceLabels(document, renderer.input.ResourceLabels, "metadata", "labels"); err != nil {
		return err
	}
	var path []string
	switch object.GetKind() {
	case "Pod":
		path = []string{"metadata", "labels"}
	case "Deployment", "StatefulSet", "DaemonSet", "ReplicaSet", "ReplicationController", "Job":
		path = []string{"spec", "template", "metadata", "labels"}
	case "CronJob":
		path = []string{"spec", "jobTemplate", "spec", "template", "metadata", "labels"}
	default:
		return nil
	}
	// Identity labels always reach pods. Collection selectors only control telemetry.
	if err := mergeResourceLabels(document, renderer.input.ResourceLabels, path...); err != nil {
		return err
	}
	labels, _, err := unstructured.NestedStringMap(document, path...)
	if err != nil {
		return err
	}
	for key, value := range renderer.input.PodSelector {
		if labels[key] != value {
			return nil
		}
	}
	return mergeResourceLabels(document, renderer.input.PodLabels, path...)
}

func mergeResourceLabels(document map[string]any, labels map[string]string, path ...string) error {
	if len(labels) == 0 {
		return nil
	}
	existing, _, err := unstructured.NestedStringMap(document, path...)
	if err != nil {
		return err
	}
	if existing == nil {
		existing = map[string]string{}
	}
	for key, value := range labels {
		existing[key] = value
	}
	return unstructured.SetNestedStringMap(document, existing, path...)
}
