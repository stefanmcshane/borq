package helm

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"helm.sh/helm/v4/pkg/action"
	"helm.sh/helm/v4/pkg/kube"
	"helm.sh/helm/v4/pkg/registry"
	"helm.sh/helm/v4/pkg/storage/driver"
)

// ChartInput identifies one pinned chart in an HTTP or OCI repository.
type ChartInput struct {
	// Repository is the repository URL, including oci:// for registries.
	Repository string `json:"repository"`
	// Name is the chart's published name within the repository.
	Name string `json:"name"`
	// Version is the exact published chart version.
	Version string `json:"version"`
}

// ChartDefaults preserves upstream values comments for editing and reference.
type ChartDefaults struct {
	// Chart identifies the downloaded artifact.
	Chart ChartInput `json:"chart"`
	// Values is the chart's original values.yaml, including comments.
	Values string `json:"values"`
}

// ReleaseInput resolves a pinned chart into a release request. The caller supplies
// its namespace, release identity, values, and reconciliation settings.
func (input ChartInput) ReleaseInput() InstallOrUpgradeInput {
	release := InstallOrUpgradeInput{ChartRef: input.Name, RepositoryUrl: input.Repository, Version: input.Version}
	if strings.HasPrefix(input.Repository, "oci://") {
		release.ChartRef = strings.TrimRight(input.Repository, "/") + "/" + input.Name
		release.RepositoryUrl = ""
	}
	return release
}

// FetchDefaults downloads the selected chart without requiring Kubernetes access.
func FetchDefaults(ctx context.Context, input ChartInput) (ChartDefaults, error) {
	if err := ctx.Err(); err != nil {
		return ChartDefaults{}, err
	}
	temporary, err := os.MkdirTemp("", "borq-chart-*")
	if err != nil {
		return ChartDefaults{}, err
	}
	defer os.RemoveAll(temporary)
	manager := ReleaseManager{tempDir: temporary}
	if err := manager.ensureCliLayout(); err != nil {
		return ChartDefaults{}, err
	}
	settings := manager.newEnvSettings("")
	registryClient, err := registry.NewClient(registry.ClientOptWriter(io.Discard), registry.ClientOptCredentialsFile(settings.RegistryConfig))
	if err != nil {
		return ChartDefaults{}, err
	}
	_, chart, err := locateChart(settings, registryClient, input.ReleaseInput())
	if err != nil {
		return ChartDefaults{}, err
	}
	if err := ctx.Err(); err != nil {
		return ChartDefaults{}, err
	}
	result := ChartDefaults{Chart: input}
	for _, file := range chart.Raw {
		if file.Name == "values.yaml" {
			result.Values = string(file.Data)
			break
		}
	}
	return result, nil
}

// Uninstall removes only the specified owner's release. Chart CRDs are retained by Helm.
func (manager ReleaseManager) Uninstall(ctx context.Context, namespace, name, owner string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	configuration := action.NewConfiguration()
	if err := configuration.Init(manager.restClientGetter(namespace), namespace, "secrets"); err != nil {
		return err
	}
	stored, err := configuration.Releases.Last(name)
	if err == driver.ErrReleaseNotFound {
		return nil
	}
	if err != nil {
		return err
	}
	live, err := releaseRecord(stored)
	if err != nil {
		return err
	}
	if owner == "" || live.Labels["borq.run/owner"] != owner {
		return fmt.Errorf("Helm release %q belongs to another owner", name)
	}
	uninstall := action.NewUninstall(configuration)
	uninstall.IgnoreNotFound = true
	uninstall.WaitStrategy = kube.LegacyStrategy
	uninstall.Timeout = defaultActionTimeout
	_, err = uninstall.Run(name)
	return err
}
