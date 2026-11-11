package platform

import (
	"context"
	"fmt"
	"log/slog"
	"slices"

	"github.com/stefanmcshane/borq/foundations/helm"
)

const borqTraefikName = "borq-traefik"

func traefikInspectionInput(name string) helm.InstallOrUpgradeInput {
	return helm.InstallOrUpgradeInput{Namespace: SystemNamespace, ReleaseName: name, ChartRef: "traefik", RepositoryUrl: "https://traefik.github.io/charts", PodLabels: componentPodLabels()}
}

func inspectTraefikRelease(ctx context.Context, releases helm.ReleaseManager, definition component, name string) (helm.ReleaseInspection, error) {
	expected := traefikInspectionInput(name)
	if name == borqTraefikName {
		expected.ChartRef, expected.RepositoryUrl = definition.Chart, definition.Repository
		expected.SharedCrds = true
	}
	inspection, err := releases.InspectWithValues(ctx, expected)
	if err != nil {
		return inspection, err
	}
	if inspection.State.Status != "absent" && !inspection.ProvenanceMatches && name == "traefik" {
		expected.SharedCrds = true
		inspection, err = releases.InspectWithValues(ctx, expected)
		if err != nil {
			return inspection, err
		}
	}
	if inspection.State.Status != "absent" && !inspection.ProvenanceMatches {
		return inspection, fmt.Errorf("Traefik release %s/%s was not installed by Borq; refusing to adopt it", SystemNamespace, name)
	}
	return inspection, nil
}

func (input componentRun) reconcileTraefik(ctx context.Context, releases helm.ReleaseManager, definition component) (ComponentState, error) {
	state := ComponentState{Name: definition.Name, Namespace: SystemNamespace, Chart: definition.Chart, Status: "failed"}
	legacy, err := inspectTraefikRelease(ctx, releases, definition, "traefik")
	if err != nil {
		return state, err
	}
	if _, err := inspectTraefikRelease(ctx, releases, definition, borqTraefikName); err != nil {
		return state, err
	}
	if err := ensureGatewayApi(ctx, input.connection); err != nil {
		return state, err
	}
	state, err = reconcileComponent(ctx, releases, definition, input.values, input.upgrade && componentSelected(input.upgradeComponents, definition.Name), input.recoverPending || slices.Contains(input.recoverComponents, definition.Name))
	if err != nil {
		return state, err
	}
	if legacy.State.Status == "absent" {
		return state, nil
	}
	retirement := traefikInspectionInput("traefik")
	retirement.Timeout = input.values.Timeout
	retirement.RecoverPending = input.recoverPending || slices.Contains(input.recoverComponents, definition.Name)
	if err := releases.Retire(ctx, retirement); err != nil {
		state.Status = "failed"
		return state, fmt.Errorf("retire legacy Borq Traefik: %w", err)
	}
	state.Changed = true
	slog.InfoContext(ctx, "component_traefik_migrated", "namespace", SystemNamespace, "release", borqTraefikName)
	return state, nil
}
