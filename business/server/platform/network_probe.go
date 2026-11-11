package platform

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/stefanmcshane/borq/foundations/kubernetes"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// This official multi-platform image is pinned so readiness tooling cannot drift.
const networkProbeImage = "curlimages/curl:8.21.0@sha256:7c12af72ceb38b7432ab85e1a265cff6ae58e06f95539d539b654f2cfa64bb13"

type networkProbeInput struct {
	Namespace      string
	Origin         string
	Path           string
	DialAddress    string
	ExpectedStatus int
}

func probeManagementAdmin(ctx context.Context, connection kubernetes.Connection, settings Settings, origin string) error {
	client, err := networkClient(connection)
	if err != nil {
		return err
	}
	address, err := url.Parse(origin)
	if err != nil {
		return fmt.Errorf("invalid management probe origin")
	}
	// Match the API default explicitly so reapplying the typed Service does not
	// replace the stored target port with its serialized zero value.
	service := &corev1.Service{ObjectMeta: networkMetadata(SystemNamespace, managementAdminEgressName), Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeExternalName, ExternalName: "unused", Ports: []corev1.ServicePort{{Name: "https", Port: 443, TargetPort: intstr.FromInt32(443), Protocol: corev1.ProtocolTCP}}}}
	service.Annotations = map[string]string{"tailscale.com/tailnet-fqdn": address.Hostname(), "tailscale.com/tags": settings.Network.Tailscale.AdminTag}
	if err := ensureNetworkResource(ctx, client, service); err != nil {
		return err
	}
	job, err := networkProbeJob(networkProbeInput{Namespace: SystemNamespace, Origin: origin, Path: "/api/livez", DialAddress: managementAdminEgressName + "." + SystemNamespace + ".svc:443", ExpectedStatus: 200})
	if err != nil {
		return err
	}
	return runNetworkProbe(ctx, client, job)
}

func networkProbeJob(input networkProbeInput) (*batchv1.Job, error) {
	origin, err := url.Parse(input.Origin)
	if err != nil || origin.Scheme != "https" || origin.Hostname() == "" || origin.User != nil || origin.Port() != "" || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" {
		return nil, fmt.Errorf("network probe requires an HTTPS origin")
	}
	disabled, enabled, uid, deadline, backoff, ttl := false, true, int64(65532), int64(120), int32(0), int32(60)
	metadata := networkMetadata(input.Namespace, "")
	metadata.GenerateName = "borq-network-probe-"
	job := &batchv1.Job{ObjectMeta: metadata, Spec: batchv1.JobSpec{BackoffLimit: &backoff, ActiveDeadlineSeconds: &deadline, TTLSecondsAfterFinished: &ttl, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: networkLabels()}, Spec: corev1.PodSpec{
		AutomountServiceAccountToken: &disabled, RestartPolicy: corev1.RestartPolicyNever,
		SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: &enabled, RunAsUser: &uid, RunAsGroup: &uid, FSGroup: &uid, SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
		Containers: []corev1.Container{{Name: "probe", Image: networkProbeImage, ImagePullPolicy: corev1.PullIfNotPresent, Command: []string{"/bin/sh", "-ec"}, Args: []string{networkProbeScript()},
			SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: &disabled, ReadOnlyRootFilesystem: &enabled, Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
			Env:             []corev1.EnvVar{{Name: "PROBE_URL", Value: input.Origin + input.Path}, {Name: "CONNECT_TO", Value: origin.Hostname() + ":443:" + input.DialAddress}, {Name: "EXPECTED_STATUS", Value: strconv.Itoa(input.ExpectedStatus)}},
			VolumeMounts:    []corev1.VolumeMount{{Name: "temporary", MountPath: "/tmp"}},
			Resources:       corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("16Mi")}, Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")}},
		}},
		Volumes: []corev1.Volume{{Name: "temporary", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}}}},
	}}}}

	return job, nil
}

func networkProbeScript() string {
	return `umask 077
previous=""
attempt=0
while true; do
  if status="$(curl --silent --output /dev/null --write-out '%{http_code}' --connect-timeout 5 --max-time 10 --proto '=https' --noproxy '*' --connect-to "$CONNECT_TO" --url "$PROBE_URL" 2>/dev/null)"; then
    if [ "$status" = "$EXPECTED_STATUS" ]; then
      echo "Management HTTPS verification succeeded"
      exit 0
    fi
    result="unexpected HTTP status $status (expected $EXPECTED_STATUS)"
  else
    code=$?
    case "$code" in
      5|6) result="DNS lookup failed for the management egress proxy" ;;
      7) result="connection to the management egress proxy failed" ;;
      28) result="management HTTPS connection timed out" ;;
      35) result="management TLS handshake failed" ;;
      60) result="management TLS certificate verification failed" ;;
      *) result="management HTTPS request failed (curl exit $code)" ;;
    esac
  fi
  attempt=$((attempt + 1))
  if [ "$result" != "$previous" ] || [ "$((attempt % 15))" = 0 ]; then
    echo "Waiting for management HTTPS: $result"
    previous="$result"
  fi
  sleep 2
done`
}

func runNetworkProbe(ctx context.Context, client ctrlclient.Client, job *batchv1.Job) error {
	if err := client.Create(ctx, job); err != nil {
		return fmt.Errorf("create network HTTPS probe: %w", err)
	}
	probeErr := wait.PollUntilContextCancel(ctx, time.Second, true, func(ctx context.Context) (bool, error) {
		observed := &batchv1.Job{}
		if err := client.Get(ctx, ctrlclient.ObjectKeyFromObject(job), observed); err != nil {
			return false, err
		}
		if observed.UID != job.UID {
			return false, fmt.Errorf("network HTTPS probe was replaced")
		}
		for _, condition := range observed.Status.Conditions {
			if condition.Status != corev1.ConditionTrue {
				continue
			}
			if condition.Type == batchv1.JobComplete {
				return true, nil
			}
			if condition.Type == batchv1.JobFailed {
				return false, fmt.Errorf("network HTTPS probe failed; check tailnet routing, access rules, and TLS")
			}
		}
		return false, nil
	})
	cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	propagation := metav1.DeletePropagationBackground
	cleanupErr := client.Delete(cleanup, job, &ctrlclient.DeleteOptions{PropagationPolicy: &propagation, Preconditions: &metav1.Preconditions{UID: &job.UID}})
	if apierrors.IsNotFound(cleanupErr) {
		cleanupErr = nil
	}
	if err := errors.Join(probeErr, cleanupErr); err != nil {
		return fmt.Errorf("verify network HTTPS reachability: %w", err)
	}
	return nil
}
