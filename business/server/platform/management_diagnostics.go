package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/stefanmcshane/borq/foundations/kubeapps"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// ReadManagementDiagnostics reports outstanding setup resources without reading
// Secrets or container logs. Observations do not change reconciliation outcomes.
func ReadManagementDiagnostics(ctx context.Context, input ManagementInput) ([]kubeapps.ResourceStatus, error) {
	settings := input.Settings
	if settings.Namespace == "" {
		settings.Namespace = ManagementNamespace
	}
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: input.KubeconfigPath}, logger.Logger{})
	if err != nil {
		return nil, err
	}
	// These built-in kinds need no discovery. Discovery uses its own context and
	// could otherwise outlive the diagnostic request when the API is unavailable.
	mapper := meta.NewDefaultRESTMapper(nil)
	for _, group := range []struct {
		version schema.GroupVersion
		kinds   []string
	}{
		{corev1.SchemeGroupVersion, []string{"ConfigMap", "Pod", "Service", "Event"}},
		{appsv1.SchemeGroupVersion, []string{"StatefulSet", "ReplicaSet"}},
		{batchv1.SchemeGroupVersion, []string{"Job"}},
		{networkingv1.SchemeGroupVersion, []string{"Ingress"}},
	} {
		for _, kind := range group.kinds {
			mapper.Add(group.version.WithKind(kind), meta.RESTScopeNamespace)
		}
	}
	configuration := connection.RestConfig()
	client, err := ctrlclient.New(&configuration, ctrlclient.Options{Scheme: kubernetes.NewScheme(), Mapper: mapper})
	if err != nil {
		return nil, err
	}
	return readManagementDiagnostics(ctx, client, settings)
}

func readManagementDiagnostics(ctx context.Context, client ctrlclient.Client, settings Settings) ([]kubeapps.ResourceStatus, error) {
	resources := []kubeapps.ResourceStatus{}
	var stored corev1.ConfigMap
	err := client.Get(ctx, ctrlclient.ObjectKey{Namespace: settings.Namespace, Name: "borq-platform-management"}, &stored)
	if err != nil && !apierrors.IsNotFound(err) {
		return resources, err
	}
	if err == nil {
		var state Result
		if err := json.Unmarshal([]byte(stored.Data["state.json"]), &state); err != nil {
			return resources, fmt.Errorf("management component status is unreadable")
		}
		for _, component := range state.Components {
			switch component.Status {
			case "pending", "installing", "blocked", "failed", "pending-install", "pending-upgrade", "pending-rollback":
				resources = append(resources, kubeapps.ResourceStatus{Kind: "HelmRelease", Namespace: component.Namespace, Name: component.Name, State: "running", Message: "component " + component.Status})
			}
		}
	}
	for _, namespace := range []string{settings.Namespace, SystemNamespace} {
		pending := map[types.UID]bool{}
		var pods corev1.PodList
		if err := client.List(ctx, &pods, ctrlclient.InNamespace(namespace)); err != nil {
			return resources, err
		}
		for _, pod := range pods.Items {
			if pod.DeletionTimestamp != nil || !managementDiagnosticWorkload(pod.Labels, settings) {
				continue
			}
			message := managementPodDiagnostic(pod)
			if message == "" {
				continue
			}
			pending[pod.UID] = true
			resources = append(resources, kubeapps.ResourceStatus{ApiVersion: "v1", Kind: "Pod", Namespace: namespace, Name: pod.Name, State: "running", Message: message})
		}
		var statefulSets appsv1.StatefulSetList
		if err := client.List(ctx, &statefulSets, ctrlclient.InNamespace(namespace)); err != nil {
			return resources, err
		}
		for _, workload := range statefulSets.Items {
			desired := int32(1)
			if workload.Spec.Replicas != nil {
				desired = *workload.Spec.Replicas
			}
			if workload.DeletionTimestamp != nil || workload.Status.ReadyReplicas >= desired || !managementDiagnosticWorkload(workload.Labels, settings) {
				continue
			}
			pending[workload.UID] = true
			resources = append(resources, kubeapps.ResourceStatus{ApiVersion: "apps/v1", Kind: "StatefulSet", Namespace: namespace, Name: workload.Name, State: "running", Message: fmt.Sprintf("waiting for pods: %d/%d ready; inspect controller events", workload.Status.ReadyReplicas, desired)})
		}
		var replicaSets appsv1.ReplicaSetList
		if err := client.List(ctx, &replicaSets, ctrlclient.InNamespace(namespace)); err != nil {
			return resources, err
		}
		for _, workload := range replicaSets.Items {
			if workload.Spec.Replicas == nil || workload.Status.ReadyReplicas >= *workload.Spec.Replicas || workload.DeletionTimestamp != nil || !managementDiagnosticWorkload(workload.Labels, settings) {
				continue
			}
			pending[workload.UID] = true
			resources = append(resources, kubeapps.ResourceStatus{ApiVersion: "apps/v1", Kind: "ReplicaSet", Namespace: namespace, Name: workload.Name, State: "running", Message: fmt.Sprintf("waiting for pods: %d/%d ready; inspect controller events", workload.Status.ReadyReplicas, *workload.Spec.Replicas)})
		}
		var jobs batchv1.JobList
		if err := client.List(ctx, &jobs, ctrlclient.InNamespace(namespace), ctrlclient.MatchingLabels{"borq.run/network": "true"}); err != nil {
			return resources, err
		}
		for _, job := range jobs.Items {
			if job.DeletionTimestamp != nil || job.Status.Succeeded > 0 || !strings.HasPrefix(job.Name, "borq-network-probe-") {
				continue
			}
			pending[job.UID] = true
			resources = append(resources, kubeapps.ResourceStatus{ApiVersion: "batch/v1", Kind: "Job", Namespace: namespace, Name: job.Name, State: "running", Message: fmt.Sprintf("waiting for management HTTPS verification; inspect: kubectl -n %s logs job/%s", namespace, job.Name)})
		}
		if namespace == SystemNamespace {
			var service corev1.Service
			err := client.Get(ctx, ctrlclient.ObjectKey{Namespace: namespace, Name: managementAdminEgressName}, &service)
			if err != nil && !apierrors.IsNotFound(err) {
				return resources, err
			}
			if err == nil && (service.Spec.ExternalName == "unused" || service.Spec.ExternalName == "") {
				pending[service.UID] = true
				resources = append(resources, kubeapps.ResourceStatus{ApiVersion: "v1", Kind: "Service", Namespace: namespace, Name: service.Name, State: "running", Message: "waiting for the Tailscale operator to assign the management egress proxy; inspect current operator logs"})
			}
		}
		if namespace == settings.Namespace {
			var ingress networkingv1.Ingress
			err := client.Get(ctx, ctrlclient.ObjectKey{Namespace: namespace, Name: "borq-management"}, &ingress)
			if err != nil && !apierrors.IsNotFound(err) {
				return resources, err
			}
			if err == nil && len(ingress.Status.LoadBalancer.Ingress) == 0 {
				pending[ingress.UID] = true
				resources = append(resources, kubeapps.ResourceStatus{ApiVersion: "networking.k8s.io/v1", Kind: "Ingress", Namespace: namespace, Name: ingress.Name, State: "running", Message: "waiting for the Tailscale management hostname"})
			}
		}
		if len(pending) == 0 {
			continue
		}
		var events corev1.EventList
		if err := client.List(ctx, &events, ctrlclient.InNamespace(namespace)); err != nil {
			return resources, err
		}
		for _, event := range events.Items {
			if event.Type != corev1.EventTypeWarning || event.InvolvedObject.UID == "" || !pending[event.InvolvedObject.UID] {
				continue
			}
			lastSeen := event.LastTimestamp.Time
			if !event.EventTime.IsZero() && event.EventTime.After(lastSeen) {
				lastSeen = event.EventTime.Time
			}
			if event.Series != nil && event.Series.LastObservedTime.After(lastSeen) {
				lastSeen = event.Series.LastObservedTime.Time
			}
			if lastSeen.Before(time.Now().Add(-5 * time.Minute)) {
				continue
			}
			explanation := managementWarningExplanation(event.Reason)
			if event.Reason == "FailedCreate" && strings.Contains(event.Message, "violates PodSecurity") {
				explanation = "pod creation is forbidden by the namespace PodSecurity policy; inspect the controller's pod security settings"
				if strings.Contains(event.Message, "privileged") {
					explanation = "pod creation is forbidden because the namespace PodSecurity policy blocks privileged containers"
				}
			}
			if explanation != "" {
				resources = append(resources, kubeapps.ResourceStatus{ApiVersion: "v1", Kind: "Event", Namespace: namespace, Name: event.InvolvedObject.Name, State: "running", Message: "recent warning " + event.Reason + ": " + explanation})
			}
		}
	}
	slices.SortFunc(resources, func(a, b kubeapps.ResourceStatus) int {
		return strings.Compare(a.Namespace+"/"+a.Kind+"/"+a.Name+"/"+a.Message, b.Namespace+"/"+b.Kind+"/"+b.Name+"/"+b.Message)
	})
	resources = slices.Compact(resources)
	return resources[:min(len(resources), 30)], nil
}

func managementDiagnosticWorkload(labels map[string]string, settings Settings) bool {
	if labels["app.kubernetes.io/part-of"] == "borq" || labels["tailscale.com/managed"] == "true" {
		return true
	}
	for _, component := range managementCatalog(settings, false) {
		if labels["app.kubernetes.io/instance"] == component.Name {
			return true
		}
	}
	return false
}

func managementPodDiagnostic(pod corev1.Pod) string {
	if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return ""
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			return ""
		}
	}
	parts := []string{"pod is not ready"}
	for _, container := range append(slices.Clone(pod.Status.InitContainerStatuses), pod.Status.ContainerStatuses...) {
		if container.Ready {
			continue
		}
		detail := fmt.Sprintf("container %s: not ready, restarts=%d", container.Name, container.RestartCount)
		if waiting := container.State.Waiting; waiting != nil {
			switch waiting.Reason {
			case "CrashLoopBackOff", "ImagePullBackOff", "ErrImagePull", "CreateContainerConfigError", "CreateContainerError", "ContainerCreating", "PodInitializing":
				detail += ", " + waiting.Reason
			}
		}
		if terminated := container.State.Terminated; terminated != nil {
			detail += fmt.Sprintf(", exited with code %d", terminated.ExitCode)
		}
		parts = append(parts, detail)
	}
	parts = append(parts, fmt.Sprintf("inspect: kubectl -n %s describe pod %s", pod.Namespace, pod.Name))
	return strings.Join(parts, "; ")
}

// Event and container messages can contain command arguments or credentials.
// Report known reasons with our own explanations instead of copying those bodies.
func managementWarningExplanation(reason string) string {
	switch reason {
	case "HTTPSNotEnabled":
		return "enable HTTPS certificates in the Tailscale DNS settings"
	case "BackOff":
		return "a container recently failed; inspect its current status and logs"
	case "Unhealthy":
		return "a health probe recently failed; inspect the pod's readiness"
	case "FailedScheduling":
		return "the pod could not be scheduled; inspect node capacity and pod events"
	case "FailedMount", "FailedAttachVolume":
		return "storage could not be attached or mounted; inspect pod events and volume claims"
	case "Failed", "FailedCreate":
		return "Kubernetes could not create or start the workload; inspect its events"
	}
	return ""
}
