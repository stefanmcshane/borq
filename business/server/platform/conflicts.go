package platform

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/stefanmcshane/borq/foundations/helm"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeclient "k8s.io/client-go/kubernetes"
)

// targetComponentConflicts inventories existing installations before the worker
// is allowed to claim the target or create credentials. Controller labels also
// cover installations not recorded in Helm's Secret-backed release storage.
func targetComponentConflicts(ctx context.Context, connection kubernetes.Connection, releases helm.ReleaseManager, namespace string) (map[string]string, error) {
	installed, err := releases.List(ctx, "")
	if err != nil {
		return nil, err
	}
	reasons := map[string][]string{}
	for _, release := range installed {
		for _, definition := range targetCatalog(false) {
			if release.State.Chart != definition.Chart || (release.Namespace == namespace && release.State.Name == definition.Name) {
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
		addControllerConflict(reasons, namespace, "Deployment", deployment.ObjectMeta)
	}
	daemonSets, err := client.AppsV1().DaemonSets("").List(ctx, options)
	if err != nil {
		return nil, fmt.Errorf("inventory component DaemonSets: %w", err)
	}
	for _, daemonSet := range daemonSets.Items {
		addControllerConflict(reasons, namespace, "DaemonSet", daemonSet.ObjectMeta)
	}
	statefulSets, err := client.AppsV1().StatefulSets("").List(ctx, options)
	if err != nil {
		return nil, fmt.Errorf("inventory component StatefulSets: %w", err)
	}
	for _, statefulSet := range statefulSets.Items {
		addControllerConflict(reasons, namespace, "StatefulSet", statefulSet.ObjectMeta)
	}
	result := make(map[string]string, len(reasons))
	for component, messages := range reasons {
		sort.Strings(messages)
		result[component] = strings.Join(messages, "; ")
	}
	return result, nil
}

func addControllerConflict(reasons map[string][]string, namespace, kind string, metadata metav1.ObjectMeta) {
	component := metadata.Labels["app.kubernetes.io/name"]
	for _, definition := range targetCatalog(false) {
		if component != definition.Name {
			continue
		}
		releaseName := metadata.Annotations["meta.helm.sh/release-name"]
		if releaseName == "" {
			releaseName = metadata.Labels["app.kubernetes.io/instance"]
		}
		if metadata.Namespace == namespace && releaseName == definition.Name {
			return
		}
		reasons[component] = append(reasons[component], fmt.Sprintf("%s %s/%s already runs %s", kind, metadata.Namespace, metadata.Name, component))
		return
	}
}
