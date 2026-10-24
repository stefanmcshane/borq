package helm

import (
	"context"
	"fmt"
	"sort"

	"helm.sh/helm/v4/pkg/action"
)

// ReleaseLocation identifies the latest persisted revision of a Helm release
// without exposing its values, manifests, or credentials.
type ReleaseLocation struct {
	// Namespace identifies where Helm stores this release.
	Namespace string
	// State describes the current chart and revision.
	State ReleaseState
}

// List reads the latest revision of every release in a namespace. An empty
// namespace includes all namespaces. No charts are downloaded or changed.
func (manager ReleaseManager) List(ctx context.Context, namespace string) ([]ReleaseLocation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	configuration := action.NewConfiguration()
	if err := configuration.Init(manager.restClientGetter(namespace), namespace, "secrets"); err != nil {
		return nil, fmt.Errorf("initialize Helm inventory: %w", err)
	}
	records, err := configuration.Releases.ListReleases()
	if err != nil {
		return nil, fmt.Errorf("list Helm releases: %w", err)
	}
	latest := map[string]ReleaseLocation{}
	for _, stored := range records {
		record, err := releaseRecord(stored)
		if err != nil {
			return nil, err
		}
		key := record.Namespace + "/" + record.Name
		if previous, exists := latest[key]; !exists || record.Version > previous.State.Revision {
			latest[key] = ReleaseLocation{Namespace: record.Namespace, State: releaseState(record)}
		}
	}
	result := make([]ReleaseLocation, 0, len(latest))
	for _, location := range latest {
		if location.State.Status != "uninstalled" {
			result = append(result, location)
		}
	}
	sort.Slice(result, func(left, right int) bool {
		return result[left].Namespace+"/"+result[left].State.Name < result[right].Namespace+"/"+result[right].State.Name
	})
	return result, ctx.Err()
}
