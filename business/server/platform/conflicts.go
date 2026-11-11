package platform

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/stefanmcshane/borq/foundations/helm"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeclient "k8s.io/client-go/kubernetes"
)

func checkSharedComponentConflicts(ctx context.Context, connection kubernetes.Connection, kubeconfigPath string, settings Settings) error {
	if settings.Network.Tailscale != nil {
		if err := checkNetworkControllerConflicts(ctx, connection, settings, true); err != nil {
			return err
		}
	}
	return checkComponentConflicts(ctx, connection, kubeconfigPath, networkSharedCatalog(settings))
}

func checkComponentConflicts(ctx context.Context, connection kubernetes.Connection, kubeconfigPath string, definitions []component) error {
	releases, err := helm.NewReleaseManager(helm.NewReleaseManagerInput{Connection: connection, KubeconfigPath: kubeconfigPath})
	if err != nil {
		return err
	}
	defer releases.Close()
	conflicts, err := targetComponentConflicts(ctx, connection, releases, SystemNamespace, definitions)
	if err != nil {
		return err
	}
	for _, definition := range definitions {
		if reason := conflicts[definition.Name]; reason != "" {
			return fmt.Errorf("cannot install shared %s: %s", definition.Name, reason)
		}
	}
	return nil
}

// targetComponentConflicts inventories existing installations before the worker
// is allowed to claim the target or create credentials. Controller labels also
// cover installations not recorded in Helm's Secret-backed release storage.
func targetComponentConflicts(ctx context.Context, connection kubernetes.Connection, releases helm.ReleaseManager, namespace string, definitions []component) (map[string]string, error) {
	installed, err := releases.List(ctx, "")
	if err != nil {
		return nil, err
	}
	reasons := map[string][]string{}
	managementEso := false
	ownedTraefik := map[string]bool{}
	for _, release := range installed {
		if release.Namespace == ManagementNamespace && release.State.Name == "external-secrets" && release.State.Chart == "external-secrets" {
			inspection, err := releases.InspectWithValues(ctx, helm.InstallOrUpgradeInput{Namespace: release.Namespace, ReleaseName: release.State.Name, ChartRef: release.State.Chart})
			if err != nil {
				return nil, err
			}
			managementEso = inspection.Values["scopedNamespace"] == ManagementNamespace && inspection.Values["scopedRBAC"] == true && inspection.Values["controllerClass"] == ManagementNamespace
			if managementEso {
				continue
			}
		}
		if release.State.Chart == "traefik" || (release.Namespace == SystemNamespace && release.State.Name == borqTraefikName) {
			if release.Namespace == SystemNamespace && (release.State.Name == "traefik" || release.State.Name == borqTraefikName) {
				definition := sharedCatalog()[1]
				for _, candidate := range definitions {
					if candidate.Name == "traefik" {
						definition = candidate
					}
				}
				if _, err := inspectTraefikRelease(ctx, releases, definition, release.State.Name); err != nil {
					reasons["traefik"] = append(reasons["traefik"], err.Error())
				} else {
					ownedTraefik[release.State.Name] = true
				}
			}
			continue
		}
		for _, definition := range targetCatalog(false) {
			expectedNamespace := namespace
			if release.State.Chart != definition.Chart || (release.Namespace == expectedNamespace && release.State.Name == definition.Name) {
				continue
			}
			reasons[definition.Name] = append(reasons[definition.Name], fmt.Sprintf("Helm release %s/%s already installs chart %s", release.Namespace, release.State.Name, release.State.Chart))
		}
	}
	configuration := connection.RestConfig()
	// Kubernetes requires a REST configuration pointer when constructing clients.
	client, err := kubeclient.NewForConfig(&configuration)
	if err != nil {
		return nil, fmt.Errorf("create controller inventory client: %w", err)
	}
	selector := "app.kubernetes.io/name in (cert-manager,external-secrets,traefik,alloy,snapshot-controller)"
	options := metav1.ListOptions{LabelSelector: selector}
	deployments, err := client.AppsV1().Deployments("").List(ctx, options)
	if err != nil {
		return nil, fmt.Errorf("inventory component Deployments: %w", err)
	}
	for _, deployment := range deployments.Items {
		if deployment.Namespace == ManagementNamespace && deployment.Name == "external-secrets-mgmt" {
			for _, container := range deployment.Spec.Template.Spec.Containers {
				if slices.Contains(container.Args, "--namespace="+ManagementNamespace) && slices.Contains(container.Args, "--controller-class="+ManagementNamespace) {
					managementEso = true
				}
			}
			if managementEso {
				continue
			}
		}
		addControllerConflict(reasons, namespace, "Deployment", deployment.ObjectMeta, ownedTraefik)
	}
	daemonSets, err := client.AppsV1().DaemonSets("").List(ctx, options)
	if err != nil {
		return nil, fmt.Errorf("inventory component DaemonSets: %w", err)
	}
	for _, daemonSet := range daemonSets.Items {
		addControllerConflict(reasons, namespace, "DaemonSet", daemonSet.ObjectMeta, ownedTraefik)
	}
	statefulSets, err := client.AppsV1().StatefulSets("").List(ctx, options)
	if err != nil {
		return nil, fmt.Errorf("inventory component StatefulSets: %w", err)
	}
	for _, statefulSet := range statefulSets.Items {
		addControllerConflict(reasons, namespace, "StatefulSet", statefulSet.ObjectMeta, ownedTraefik)
	}
	result := make(map[string]string, len(reasons))
	for component, messages := range reasons {
		sort.Strings(messages)
		result[component] = strings.Join(messages, "; ")
	}
	return result, nil
}

func addControllerConflict(reasons map[string][]string, namespace, kind string, metadata metav1.ObjectMeta, ownedTraefik map[string]bool) {
	component := metadata.Labels["app.kubernetes.io/name"]
	for _, definition := range targetCatalog(false) {
		if component != definition.Name {
			continue
		}

		releaseName := metadata.Annotations["meta.helm.sh/release-name"]
		if releaseName == "" {
			releaseName = metadata.Labels["app.kubernetes.io/instance"]
		}
		if component == "traefik" {
			if metadata.Namespace != SystemNamespace {
				return
			}
			if ownedTraefik[releaseName] {
				return
			}
			if metadata.Name != "traefik" && metadata.Name != borqTraefikName && releaseName != "traefik" && releaseName != borqTraefikName {
				return
			}
			reasons[component] = append(reasons[component], fmt.Sprintf("%s %s/%s has no verified Borq Helm release", kind, metadata.Namespace, metadata.Name))
			return
		}
		if metadata.Namespace == namespace && releaseName == definition.Name {
			return
		}
		reasons[component] = append(reasons[component], fmt.Sprintf("%s %s/%s already runs %s", kind, metadata.Namespace, metadata.Name, component))
		return
	}
}
