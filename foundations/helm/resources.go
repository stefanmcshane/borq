package helm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"helm.sh/helm/v4/pkg/action"
	"helm.sh/helm/v4/pkg/storage/driver"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	yamlreader "k8s.io/apimachinery/pkg/util/yaml"
)

// ResourceIdentity identifies a rendered resource without exposing its configuration.
type ResourceIdentity struct {
	// Kind is the Kubernetes resource kind.
	Kind string
	// Namespace scopes the resource; omitted manifest namespaces use the release namespace.
	Namespace string
	// Name is the rendered resource name, which may differ from the release name.
	Name string
}

// ResourceHistory reads identities from retained, owned release revisions and hooks.
// Keeping older names lets callers correlate events after upgrades remove resources.
func (manager ReleaseManager) ResourceHistory(ctx context.Context, namespace, name, owner string) ([]ResourceIdentity, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if namespace == "" || name == "" || owner == "" {
		return nil, errors.New("release namespace, name and owner are required")
	}
	configuration := action.NewConfiguration()
	getter := manager.restClientGetter(namespace)
	// Helm's storage API does not accept a context. Bound its underlying read
	// transport so an unreachable cluster cannot leave a monitoring panel hanging.
	getter.restConfig.Timeout = 10 * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		getter.restConfig.Timeout = min(getter.restConfig.Timeout, time.Until(deadline))
	}
	if err := configuration.Init(getter, namespace, "secrets"); err != nil {
		return nil, fmt.Errorf("initialize Helm resource history: %w", err)
	}
	releases, err := configuration.Releases.History(name)
	if errors.Is(err, driver.ErrReleaseNotFound) {
		return []ResourceIdentity{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read Helm resource history: %w", err)
	}
	resources := []ResourceIdentity{}
	for _, stored := range releases {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		release, err := releaseRecord(stored)
		if err != nil {
			return nil, err
		}
		if release.Labels["borq.run/owner"] != owner {
			continue
		}
		manifests := []string{release.Manifest}
		for _, hook := range release.Hooks {
			manifests = append(manifests, hook.Manifest)
		}
		for _, manifest := range manifests {
			identities, err := manifestIdentities(manifest, namespace)
			if err != nil {
				return nil, err
			}
			for _, identity := range identities {
				if !slices.Contains(resources, identity) {
					resources = append(resources, identity)
				}
			}
		}
	}
	return resources, nil
}

func manifestIdentities(manifest, namespace string) ([]ResourceIdentity, error) {
	decoder := yamlreader.NewYAMLOrJSONDecoder(strings.NewReader(manifest), 4096)
	resources := []ResourceIdentity{}
	var appendObject func(unstructured.Unstructured) error
	appendObject = func(object unstructured.Unstructured) error {
		if object.IsList() {
			return object.EachListItem(func(item runtime.Object) error {
				return appendObject(*item.(*unstructured.Unstructured))
			})
		}
		if object.GetName() == "" || object.GetKind() == "" {
			return nil
		}
		identity := ResourceIdentity{Kind: object.GetKind(), Name: object.GetName(), Namespace: object.GetNamespace()}
		if identity.Namespace == "" {
			identity.Namespace = namespace
		}
		resources = append(resources, identity)
		return nil
	}
	for {
		var object unstructured.Unstructured
		if err := decoder.Decode(&object.Object); err == io.EOF {
			return resources, nil
		} else if err != nil {
			return nil, fmt.Errorf("decode Helm resource identity: %w", err)
		}
		if err := appendObject(object); err != nil {
			return nil, err
		}
	}
}
