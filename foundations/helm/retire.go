package helm

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"helm.sh/helm/v4/pkg/action"
	"helm.sh/helm/v4/pkg/kube"
	"helm.sh/helm/v4/pkg/storage/driver"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/yaml"
)

// Retire removes an obsolete Borq release after its replacement is ready. It
// verifies stored provenance and live ownership, preserving shared definitions,
// namespaces, persistent volumes and resources now owned by another release.
func (manager ReleaseManager) Retire(ctx context.Context, input InstallOrUpgradeInput) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	configuration := action.NewConfiguration()
	if err := configuration.Init(manager.restClientGetter(input.Namespace), input.Namespace, "secrets"); err != nil {
		return err
	}
	stored, err := configuration.Releases.Last(input.ReleaseName)
	if errors.Is(err, driver.ErrReleaseNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	live, err := releaseRecord(stored)
	if err != nil {
		return err
	}
	if !matchesProvenance(live, input) {
		input.SharedCrds = !input.SharedCrds
	}
	if !matchesProvenance(live, input) {
		return fmt.Errorf("cannot retire %s/%s: release provenance does not match Borq", input.Namespace, input.ReleaseName)
	}
	if live.Info != nil && live.Info.Status.IsPending() && !input.RecoverPending {
		return fmt.Errorf("cannot retire %s/%s: another Helm operation is pending", input.Namespace, input.ReleaseName)
	}
	resources, err := configuration.KubeClient.Build(strings.NewReader(live.Manifest), false)
	if err != nil {
		return err
	}
	var manifest strings.Builder
	for _, resource := range resources {
		if err := ctx.Err(); err != nil {
			return err
		}
		switch resource.Mapping.GroupVersionKind.Kind {
		case "CustomResourceDefinition", "Namespace", "PersistentVolumeClaim", "PersistentVolume":
			continue
		}
		if err := resource.Get(); apierrors.IsNotFound(err) {
			continue
		} else if err != nil {
			return err
		}
		metadata, err := meta.Accessor(resource.Object)
		if err != nil {
			return err
		}
		annotations := metadata.GetAnnotations()
		if annotations["meta.helm.sh/release-name"] != input.ReleaseName || annotations["meta.helm.sh/release-namespace"] != input.Namespace {
			continue
		}
		encoded, err := yaml.Marshal(resource.Object)
		if err != nil {
			return err
		}
		manifest.WriteString("---\n")
		manifest.Write(encoded)
	}
	// A failed install can record objects that it never acquired. Prune those
	// from the deletion manifest before Helm runs; never delete by name alone.
	live.Manifest = manifest.String()
	if err := configuration.Releases.Update(live); err != nil {
		return err
	}
	uninstall := action.NewUninstall(configuration)
	uninstall.DisableHooks = true
	uninstall.IgnoreNotFound = true
	uninstall.WaitStrategy = kube.LegacyStrategy
	uninstall.Timeout = timeoutForInput(input.Timeout)
	_, err = uninstall.Run(input.ReleaseName)
	return err
}
