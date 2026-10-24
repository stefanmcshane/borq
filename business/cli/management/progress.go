package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
)

type managementProgress struct {
	pods            []string
	volumes         []string
	components      []string
	managementPhase string
	serverAttempt   int
	failure         string
}

func waitForManagementServer(ctx context.Context, log logger.Logger, request Request, connection kubernetes.Connection) error {
	configuration := connection.RestConfig()
	// The Kubernetes SDK requires a configuration pointer to construct its client.
	client, err := coreclient.NewForConfig(&configuration)
	if err != nil {
		return fmt.Errorf("create bootstrap progress client: %w", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	completed := make(chan error, 1)
	go func() {
		err := connection.Deployments().WaitForRollout(ctx, request.Namespace, borqName)
		if err == nil {
			err = probeBorq(ctx, request.ProbeURL)
		}
		completed <- err
	}()
	probe := &http.Client{Timeout: 2 * time.Second}
	started := time.Now()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		progress := readManagementProgress(ctx, client, probe, request)
		log.Info("management_bootstrap_progress", logger.WithFields(
			logger.Duration("elapsed", time.Since(started).Round(time.Second)),
			logger.String("health_url", request.ProbeURL),
			logger.String("pods", strings.Join(progress.pods, "; ")),
			logger.String("volumes", strings.Join(progress.volumes, "; ")),
			logger.String("components", strings.Join(progress.components, "; ")),
			logger.String("management_phase", progress.managementPhase),
			logger.Int("server_attempt", progress.serverAttempt)))
		if progress.failure != "" {
			return errors.New(progress.failure)
		}
		select {
		case err := <-completed:
			if err == nil {
				return nil
			}
			return fmt.Errorf("management bootstrap: %w; last observed: phase=%s, attempt=%d, pods=[%s], volumes=[%s], components=[%s]. The installation remains in the cluster; rerun management bootstrap to resume waiting",
				err, progress.managementPhase, progress.serverAttempt, strings.Join(progress.pods, "; "), strings.Join(progress.volumes, "; "), strings.Join(progress.components, "; "))
		case <-ticker.C:
		}
	}
}

// The SDK interface accepts both its live REST client and its official test client.
// The HTTP client pointer reuses the configured transport across progress probes.
func readManagementProgress(ctx context.Context, client coreclient.CoreV1Interface, probe *http.Client, request Request) managementProgress {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	progress := managementProgress{managementPhase: "api_unreachable"}
	pods, err := client.Pods(request.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		progress.pods = []string{"unavailable"}
	} else {
		for _, pod := range pods.Items {
			progress.pods = append(progress.pods, managementPodProgress(pod))
			if incompatibleServerImage(ctx, client, pod, request) {
				progress.failure = fmt.Sprintf("server image %q does not support the Borq server command; use the complete local bundle or provide --server-image with a compatible version", request.BorqImage)
			}
		}
		if len(progress.pods) == 0 {
			progress.pods = []string{"no pods created yet"}
		}
	}
	volumes, err := client.PersistentVolumeClaims(request.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		progress.volumes = []string{"unavailable"}
	} else {
		for _, volume := range volumes.Items {
			progress.volumes = append(progress.volumes, fmt.Sprintf("%s/%s: %s", volume.Namespace, volume.Name, volume.Status.Phase))
		}
	}
	state, err := client.ConfigMaps(request.Namespace).Get(ctx, "borq-platform-management", metav1.GetOptions{})
	if err == nil {
		var saved struct {
			Components []struct{ Name, Status string }
		}
		if json.Unmarshal([]byte(state.Data["state.json"]), &saved) == nil {
			for _, component := range saved.Components {
				progress.components = append(progress.components, component.Name+": "+component.Status)
			}
		} else {
			progress.components = []string{"saved state unreadable"}
		}
	} else if !apierrors.IsNotFound(err) {
		progress.components = []string{"unavailable"}
	}
	sort.Strings(progress.pods)
	sort.Strings(progress.volumes)
	sort.Strings(progress.components)
	healthRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, request.ProbeURL, nil)
	if err != nil {
		return progress
	}
	response, err := probe.Do(healthRequest)
	if err != nil {
		return progress
	}
	defer response.Body.Close()
	progress.managementPhase = fmt.Sprintf("http_%d", response.StatusCode)
	var health struct {
		Checks []struct {
			Name    string
			Phase   string
			Attempt int
		}
	}
	if json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&health) == nil {
		for _, check := range health.Checks {
			if check.Name == "management" && check.Phase != "" {
				progress.managementPhase = check.Phase
				progress.serverAttempt = check.Attempt
			}
		}
	}
	return progress
}

// Inspect only the requested server container, and report a fixed diagnosis
// rather than copying server logs, which may contain credentials, into output.
func incompatibleServerImage(ctx context.Context, client coreclient.CoreV1Interface, pod corev1.Pod, request Request) bool {
	if pod.DeletionTimestamp != nil || pod.Labels["app.kubernetes.io/name"] != borqName {
		return false
	}
	matching := false
	for _, container := range pod.Spec.Containers {
		if container.Name == borqName && container.Image == request.BorqImage && slices.Equal(container.Args, borqServerArguments(request)) {
			matching = true
		}
	}
	if !matching {
		return false
	}
	for _, container := range pod.Status.ContainerStatuses {
		if container.Name != borqName {
			continue
		}
		previous := false
		if container.State.Terminated != nil && container.State.Terminated.ExitCode != 0 {
			previous = false
		} else if container.State.Waiting != nil && container.LastTerminationState.Terminated != nil && container.LastTerminationState.Terminated.ExitCode != 0 {
			previous = true
		} else {
			continue
		}
		// The Kubernetes log API requires a pointer for its optional line limit.
		lines := int64(20)
		stream, err := client.Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{Container: borqName, Previous: previous, TailLines: &lines}).Stream(ctx)
		if err != nil {
			return false
		}
		defer stream.Close()
		output, err := io.ReadAll(io.LimitReader(stream, 4096))
		return err == nil && strings.Contains(string(output), `unknown command "server" for "borq"`)
	}
	return false
}

func managementPodProgress(pod corev1.Pod) string {
	phase := string(pod.Status.Phase)
	if pod.DeletionTimestamp != nil {
		phase = "Terminating"
	}
	ready, restarts := 0, int32(0)
	for _, container := range pod.Status.InitContainerStatuses {
		restarts += container.RestartCount
		if container.State.Waiting != nil {
			phase = "Init:" + container.State.Waiting.Reason
		} else if container.State.Terminated != nil && container.State.Terminated.ExitCode != 0 {
			phase = fmt.Sprintf("Init:ExitCode%d", container.State.Terminated.ExitCode)
		}
	}
	for _, container := range pod.Status.ContainerStatuses {
		restarts += container.RestartCount
		if container.Ready {
			ready++
		}
		if container.State.Waiting != nil {
			phase = container.State.Waiting.Reason
		} else if container.State.Terminated != nil && container.State.Terminated.ExitCode != 0 {
			phase = fmt.Sprintf("ExitCode%d", container.State.Terminated.ExitCode)
		}
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionFalse {
			phase = condition.Reason
		}
	}
	return fmt.Sprintf("%s/%s: %s (%d/%d ready, %d restarts)", pod.Namespace, pod.Name, phase, ready, len(pod.Spec.Containers), restarts)
}
