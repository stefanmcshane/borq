//go:build integration

package platform

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// TestNetworkProbeIntegration exercises the production probe in real pods against
// a TLS server. Only the fixture CA is added to curl's trust store; hostname/SNI,
// redirect rejection and cleanup remain real boundaries.
// It does not claim that the absent Tailscale controller has established routing.
func TestNetworkProbeIntegration(t *testing.T) {
	is := is.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	_, client, settings, kubeconfig := networkReconciliationFixture(t, ctx)
	namespace := settings.Namespace
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	is.NoErr(err)
	certificate := x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"route.test.ts.net"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, &certificate, &certificate, &key.PublicKey, key)
	is.NoErr(err)
	encodedKey, err := x509.MarshalECPrivateKey(key)
	is.NoErr(err)
	certificatePem := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	is.NoErr(client.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "fixture-tls"}, Data: map[string][]byte{"tls.crt": certificatePem, "tls.key": pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: encodedKey})}}))
	config := `pid /tmp/nginx.pid;
error_log /dev/null;
events {}
http {
 access_log off;
 client_body_temp_path /tmp/client_body;
 proxy_temp_path /tmp/proxy;
 fastcgi_temp_path /tmp/fastcgi;
 uwsgi_temp_path /tmp/uwsgi;
 scgi_temp_path /tmp/scgi;
 server {
  listen 8443 ssl;
  ssl_certificate /tls/tls.crt;
  ssl_certificate_key /tls/tls.key;
  if ($ssl_server_name != route.test.ts.net) { return 421; }
  location = /api/livez { return 200; }
  location = /redirect { return 302 https://route.test.ts.net/api/livez; }
 }
}`
	is.NoErr(client.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "fixture-server"}, Data: map[string]string{"nginx.conf": config}}))
	// Reuse the probe's hardened pod defaults, replacing only the fixture process
	// and its mounts. No service-account credential is needed by either workload.
	fixtureJob, err := networkProbeJob(networkProbeInput{Namespace: namespace, Origin: "https://route.test.ts.net", Path: "/api/livez", DialAddress: "fixture-server:8443", ExpectedStatus: 200})
	is.NoErr(err)
	server := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "fixture-server", Labels: map[string]string{"fixture": "network-probe"}}, Spec: fixtureJob.Spec.Template.Spec}
	server.Spec.Containers[0].Image = "nginx/nginx-unprivileged:1.30.6-alpine-slim@sha256:984a21e29bb10d4c0e122374b97b89a2101823a9331e0f3c6c04bc285ccf447f"
	server.Spec.Containers[0].Command = []string{"nginx"}
	server.Spec.Containers[0].Args = []string{"-c", "/config/nginx.conf", "-g", "daemon off;"}
	server.Spec.Containers[0].Env = nil
	server.Spec.Containers[0].VolumeMounts = append(server.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{Name: "tls", MountPath: "/tls", ReadOnly: true}, corev1.VolumeMount{Name: "config", MountPath: "/config", ReadOnly: true})
	server.Spec.Volumes = append(server.Spec.Volumes, corev1.Volume{Name: "tls", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "fixture-tls"}}}, corev1.Volume{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "fixture-server"}}}})
	is.NoErr(client.Create(ctx, server))
	is.NoErr(client.Create(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "fixture-server"}, Spec: corev1.ServiceSpec{Selector: server.Labels, Ports: []corev1.ServicePort{{Port: 443, TargetPort: intstr.FromInt32(8443)}}}}))
	is.NoErr(wait.PollUntilContextCancel(ctx, time.Second, true, func(ctx context.Context) (bool, error) {
		if err := client.Get(ctx, ctrlclient.ObjectKeyFromObject(server), server); err != nil {
			return false, err
		}
		for _, condition := range server.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
				return true, nil
			}
		}
		return false, nil
	}))
	input := networkProbeInput{Namespace: namespace, Origin: "https://route.test.ts.net", Path: "/api/livez", DialAddress: "fixture-server." + namespace + ".svc:443", ExpectedStatus: 200}
	settings.Network.Cloudflare = nil
	settings.Network.Tailscale.ManagementHostname = "route"
	ingress := managementNetworkIngress(settings)
	is.NoErr(ensureNetworkIngress(ctx, client, settings, ingress))
	is.NoErr(client.Get(ctx, ctrlclient.ObjectKeyFromObject(ingress), ingress))
	ingress.Status.LoadBalancer.Ingress = []networkingv1.IngressLoadBalancerIngress{{Hostname: "route.test.ts.net"}}
	is.NoErr(client.Status().Update(ctx, ingress))
	state, err := ReadManagementNetwork(ctx, ManagementInput{KubeconfigPath: kubeconfig, Settings: settings})
	is.NoErr(err)
	is.Equal(state, NetworkState{})
	job := networkProbeWithFixtureTrust(t, input)
	is.NoErr(runNetworkProbe(ctx, client, job))
	is.NoErr(recordNetworkProof(ctx, client, settings, NetworkState{ManagementURL: input.Origin}))
	state, err = ReadManagementNetwork(ctx, ManagementInput{KubeconfigPath: kubeconfig, Settings: settings})
	is.NoErr(err)
	is.Equal(state, NetworkState{ManagementURL: input.Origin})
	for _, failure := range []string{"certificate hostname", "redirect"} {
		t.Run(failure, func(t *testing.T) {
			is := is.New(t)
			invalid := input
			switch failure {
			case "certificate hostname":
				invalid.Origin = "https://wrong.test.ts.net"
			case "redirect":
				invalid.Path = "/redirect"
			}
			job := networkProbeWithFixtureTrust(t, invalid)
			deadline := int64(6)
			job.Spec.ActiveDeadlineSeconds = &deadline
			err := runNetworkProbe(ctx, client, job)
			is.True(err != nil && strings.Contains(err.Error(), "network HTTPS probe failed"))
			jobs := &batchv1.JobList{}
			is.NoErr(client.List(ctx, jobs, ctrlclient.InNamespace(namespace)))
			is.Equal(len(jobs.Items), 0)
		})
	}
}

func networkProbeWithFixtureTrust(t *testing.T, input networkProbeInput) *batchv1.Job {
	t.Helper()
	job, err := networkProbeJob(input)
	if err != nil {
		t.Fatal(err)
	}
	job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes, corev1.Volume{Name: "fixture-ca", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "fixture-tls", Items: []corev1.KeyToPath{{Key: "tls.crt", Path: "ca.crt"}}}}})
	job.Spec.Template.Spec.Containers[0].VolumeMounts = append(job.Spec.Template.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{Name: "fixture-ca", MountPath: "/fixture-ca", ReadOnly: true})
	job.Spec.Template.Spec.Containers[0].Env = append(job.Spec.Template.Spec.Containers[0].Env, corev1.EnvVar{Name: "CURL_CA_BUNDLE", Value: "/fixture-ca/ca.crt"})
	return job
}

// TestNetworkTailscaleOnlyIntegration verifies provider omission reaches the
// actual write boundary: only the private admin Ingress is created.
func TestNetworkTailscaleOnlyIntegration(t *testing.T) {
	is := is.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, client, settings, kubeconfig := networkReconciliationFixture(t, ctx)
	settings.Network.Cloudflare = nil
	settings.Timeout = 500 * time.Millisecond
	_, err := ReconcileManagementNetwork(ctx, ManagementInput{KubeconfigPath: kubeconfig, Settings: settings})
	is.True(err != nil && strings.Contains(err.Error(), "wait for management tailnet hostname"))
	ingresses := &networkingv1.IngressList{}
	is.NoErr(client.List(ctx, ingresses, ctrlclient.InNamespace(settings.Namespace)))
	is.Equal(len(ingresses.Items), 1)
	is.Equal(ingresses.Items[0].Name, "borq-management")
	is.Equal(ingresses.Items[0].Spec.TLS[0].Hosts, []string{fmt.Sprintf("borq-%s", settings.ClusterName)})
}
