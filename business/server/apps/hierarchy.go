package apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/sqlite"
)

// Organization owns independent spaces and cluster registrations.
type Organization struct {
	// Name is unique within this Borq server.
	Name string `json:"name"`
}

// CreateOrganization creates an organization without allocating deployment spaces.
func (manager Manager) CreateOrganization(ctx context.Context, name string) (Organization, error) {
	if err := validateNames(name); err != nil {
		return Organization{}, err
	}
	organization := Organization{Name: name}
	if previous, _, err := loadDocument[Organization](ctx, manager.database, "organization", name); err == nil {
		return previous, nil
	} else if !errors.Is(err, ErrNotFound) {
		return Organization{}, err
	}
	organizationWrite, err := documentWrite("organization", name, organization, 0)
	if err != nil {
		return Organization{}, err
	}
	if err := manager.database.Commit(ctx, []sqlite.Write{organizationWrite}); err != nil {
		if errors.Is(err, sqlite.ErrConflict) {
			previous, _, readErr := loadDocument[Organization](ctx, manager.database, "organization", name)
			return previous, readErr
		}
		return Organization{}, err
	}
	return organization, nil
}

// ListOrganizations returns organizations in name order, including those without clusters.
func (manager Manager) ListOrganizations(ctx context.Context) ([]Organization, error) {
	records, err := manager.database.List(ctx, "organization")
	if err != nil {
		return nil, err
	}
	organizations := []Organization{}
	for _, record := range records {
		var organization Organization
		if err := json.Unmarshal(record.Value, &organization); err != nil {
			return nil, fmt.Errorf("decode organization: %w", err)
		}
		organizations = append(organizations, organization)
	}
	sort.Slice(organizations, func(i, j int) bool { return organizations[i].Name < organizations[j].Name })
	return organizations, nil
}

// ListClusters returns only registrations owned by the selected organization.
func (manager Manager) ListClusters(ctx context.Context, organization string) ([]Cluster, error) {
	if _, _, err := loadDocument[Organization](ctx, manager.database, "organization", organization); err != nil {
		return nil, err
	}
	records, err := manager.database.List(ctx, "cluster")
	if err != nil {
		return nil, err
	}
	clusters := []Cluster{}
	for _, record := range records {
		var cluster Cluster
		if err := json.Unmarshal(record.Value, &cluster); err != nil {
			return nil, fmt.Errorf("decode cluster: %w", err)
		}
		if cluster.Organization == organization {
			cluster.ID = platform.ClusterID(cluster.Organization, cluster.Name)
			cluster.Kubeconfig = ""
			clusters = append(clusters, cluster)
		}
	}
	sort.Slice(clusters, func(i, j int) bool { return clusters[i].Name < clusters[j].Name })
	return clusters, nil
}

func initialSpace(organization, name string) Space {
	id := stableId(spaceKey(organization, name))
	return Space{ID: id, Organization: organization, Name: name, Namespace: "borq-" + id[:24], CertificateIssuer: "borq", Configuration: Configuration{Apps: []App{}}}
}

// CreateSpace binds a space to one cluster with an automatic namespace and issuer.
// Namespace ownership is persisted here; Kubernetes resources are created during apply.
func (manager Manager) CreateSpace(ctx context.Context, input SpaceInput) (Space, error) {
	manager.runtime.registrationMutex.Lock()
	defer manager.runtime.registrationMutex.Unlock()
	if err := validateNames(input.Organization, input.Name, input.Cluster); err != nil {
		return Space{}, err
	}
	if _, _, err := loadDocument[Organization](ctx, manager.database, "organization", input.Organization); err != nil {
		return Space{}, err
	}
	if _, _, err := loadDocument[Cluster](ctx, manager.database, "cluster", input.Organization+"/"+input.Cluster); err != nil {
		return Space{}, err
	}
	key := spaceKey(input.Organization, input.Name)
	stored, previous, err := loadDocument[spaceRecord](ctx, manager.database, "space", key)
	exists := err == nil
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Space{}, err
	}
	if !exists {
		stored = spaceRecord{Space: initialSpace(input.Organization, input.Name), Manifests: []map[string]any{}}
	}
	space := stored.Space
	space.Cluster = input.Cluster
	if exists && stored.Space.Cluster != "" {
		if space.Cluster != stored.Space.Cluster {
			return Space{}, fmt.Errorf("%w: space already has a different cluster", ErrConflict)
		}
		return stored.Space, nil
	}
	stored.Space = space
	spaceWrite, err := documentWrite("space", key, stored, previous.Revision)
	if err != nil {
		return Space{}, err
	}
	bindingWrite, err := documentWrite("namespace", space.Organization+"/"+space.Cluster+"/"+space.Namespace, map[string]string{"spaceId": space.ID}, 0)
	if err != nil {
		return Space{}, err
	}
	if err := manager.database.Commit(ctx, []sqlite.Write{spaceWrite, bindingWrite}); err != nil {
		if errors.Is(err, sqlite.ErrConflict) {
			current, _, readErr := loadDocument[spaceRecord](ctx, manager.database, "space", key)
			if readErr == nil && current.Space.Cluster == space.Cluster && current.Space.Namespace == space.Namespace && current.Space.CertificateIssuer == space.CertificateIssuer {
				return current.Space, nil
			}
		}
		return Space{}, translateStoreError(err)
	}
	return space, nil
}

// GetSpace returns accepted configuration even when its latest rollout failed.
func (manager Manager) GetSpace(ctx context.Context, organization, name string) (Space, error) {
	record, _, err := loadDocument[spaceRecord](ctx, manager.database, "space", spaceKey(organization, name))
	if err != nil {
		return Space{}, err
	}
	record.Space.CertificateIssuer, err = normalizeCertificateIssuer(record.Space.CertificateIssuer)
	return record.Space, err
}

// ListApps reads one accepted space snapshot without rendering, resolving env versions, or contacting Kubernetes.
func (manager Manager) ListApps(ctx context.Context, organization, space string) (ApplicationState, error) {
	record, _, err := loadDocument[spaceRecord](ctx, manager.database, "space", spaceKey(organization, space))
	if err != nil {
		return ApplicationState{}, err
	}
	state := ApplicationState{Configuration: record.Space.Configuration, Manifests: record.Manifests}
	if state.Configuration.Apps == nil {
		state.Configuration.Apps = []App{}
	}
	if state.Manifests == nil {
		state.Manifests = []map[string]any{}
	}
	return state, nil
}

// ListSpaces returns only the selected organization's deployment spaces.
func (manager Manager) ListSpaces(ctx context.Context, organization string) ([]Space, error) {
	if _, _, err := loadDocument[Organization](ctx, manager.database, "organization", organization); err != nil {
		return nil, err
	}
	records, err := manager.database.List(ctx, "space")
	if err != nil {
		return nil, err
	}
	spaces := []Space{}
	for _, record := range records {
		var stored spaceRecord
		if err := json.Unmarshal(record.Value, &stored); err != nil {
			return nil, fmt.Errorf("decode space: %w", err)
		}
		if stored.Space.Organization == organization {
			spaces = append(spaces, stored.Space)
		}
	}
	sort.Slice(spaces, func(i, j int) bool { return spaces[i].Name < spaces[j].Name })
	return spaces, nil
}
