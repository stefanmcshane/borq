package apps

import (
	"encoding/json"
	"fmt"

	"github.com/stefanmcshane/borq/foundations/kubeapps"
)

func plannedNamespace(plan Plan) (kubeapps.NamespaceInput, error) {
	// Decode the saved values rather than recomputing ownership under newer code.
	// Plans written before ownership metadata was added retain their target label.
	encoded, err := json.Marshal(plan.Values["resourceMetadata"])
	if err != nil {
		return kubeapps.NamespaceInput{}, fmt.Errorf("read planned namespace metadata: %w", err)
	}
	var metadata resourceMetadata
	if err := json.Unmarshal(encoded, &metadata); err != nil {
		return kubeapps.NamespaceInput{}, fmt.Errorf("decode planned namespace metadata: %w", err)
	}
	return kubeapps.NamespaceInput{Name: plan.Space.Namespace, TargetID: plan.Space.ID, Labels: metadata.Labels, Annotations: metadata.Annotations}, nil
}
