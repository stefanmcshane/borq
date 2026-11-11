//go:build integration

package appserver_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	acmev1 "github.com/cert-manager/cert-manager/pkg/apis/acme/v1"
	certv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	certmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	certclient "github.com/cert-manager/cert-manager/pkg/client/clientset/versioned"
	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/api/appapi"
	"github.com/stefanmcshane/borq/business/server/apps"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayclient "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"
	"sigs.k8s.io/yaml"
)

// TestApplicationIntegration deploys a YAML fixture through the real JSON API
// and Kubernetes, then verifies ownership, live inspection, TLS and drift repair.
// The local fixture owns its SQLite state. Remote runs require a real managed provider and an administrator credential.
func TestApplicationIntegration(t *testing.T) {
	is := is.New(t)
	kubeconfigPath := os.Getenv("BORQ_KUBECONFIG")
	serverUrl := os.Getenv("BORQ_INTEGRATION_SERVER_URL")
	if serverUrl != "" {
		request, err := http.NewRequest(http.MethodGet, strings.TrimRight(serverUrl, "/")+"/api/auth/session", nil)
		is.NoErr(err)
		authenticateApplicationTestRequest(t, request)
	}
	image := os.Getenv("BORQ_APPLICATION_TEST_IMAGE")
	ingressUrl := os.Getenv("BORQ_APPLICATION_TEST_INGRESS_URL")
	if kubeconfigPath == "" {
		t.Fatal("BORQ_KUBECONFIG is required for application integration tests")
	}
	if image == "" {
		image = "nginx:1.27.5"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	suffix := fmt.Sprintf("%x", time.Now().UnixNano())
	organization, space, cluster := "integration-"+suffix, "local", "local"
	otherOrganization := "other-" + suffix
	if serverUrl == "" {
		manager, err := apps.NewManager(ctx, apps.NewInput{
			DatabasePath:   filepath.Join(t.TempDir(), "applications.sqlite"),
			RolloutTimeout: 90 * time.Second, PollInterval: 100 * time.Millisecond,
		})
		is.NoErr(err)
		for _, name := range []string{organization, otherOrganization} {
			_, err := manager.CreateOrganization(ctx, name)
			is.NoErr(err)
		}
		handler, err := appapi.NewHandler(manager)
		is.NoErr(err)
		server := httptest.NewServer(handler)
		t.Cleanup(func() {
			server.Close()
			is.NoErr(manager.Close())
		})
		serverUrl = server.URL
	}
	client, err := appapi.NewClientWithResponses(strings.TrimRight(serverUrl, "/"), appapi.WithHTTPClient(applicationTestHttpClient()), appapi.WithRequestEditorFn(func(_ context.Context, request *http.Request) error {
		authenticateApplicationTestRequest(t, request)
		return nil
	}))
	is.NoErr(err)
	kubeconfig, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	is.NoErr(err)
	kube, err := kubernetes.NewForConfig(kubeconfig)
	is.NoErr(err)
	certificates, err := certclient.NewForConfig(kubeconfig)
	is.NoErr(err)
	gateways, err := gatewayclient.NewForConfig(kubeconfig)
	is.NoErr(err)
	if ingressUrl == "" {
		service, err := kube.CoreV1().Services("borq-system").Get(ctx, "borq-traefik", metav1.GetOptions{})
		is.NoErr(err)
		var httpsPort int32
		for _, port := range service.Spec.Ports {
			if port.Name == "websecure" {
				httpsPort = port.NodePort
			}
		}
		nodes, err := kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		is.NoErr(err)
		var nodeAddress string
		for _, node := range nodes.Items {
			for _, address := range node.Status.Addresses {
				if address.Type == corev1.NodeInternalIP {
					nodeAddress = address.Address
					break
				}
			}
			if nodeAddress != "" {
				break
			}
		}
		if httpsPort == 0 || nodeAddress == "" {
			t.Fatal("Borq Traefik requires a websecure NodePort and a node InternalIP, or BORQ_APPLICATION_TEST_INGRESS_URL")
		}
		ingressUrl = "https://" + net.JoinHostPort(nodeAddress, fmt.Sprint(httpsPort))
	}
	issuer := "borq"
	if os.Getenv("BORQ_INTEGRATION_SERVER_URL") != "" {
		for _, name := range []string{organization, otherOrganization} {
			response, err := client.PutOrganizationWithResponse(ctx, name, appapi.OrganizationOnboardingInput{})
			is.NoErr(err)
			if response.StatusCode() != http.StatusOK {
				t.Fatalf("remote organization onboarding returned %d; use a valid BORQ_APPLICATION_ADMIN_TOKEN_FILE and prepare one ready managed Pocket ID instance before running remote application acceptance", response.StatusCode())
			}
		}
	}
	emptySpaces, err := client.ListSpacesWithResponse(ctx, organization)
	is.NoErr(err)
	is.Equal(*emptySpaces.JSON200, []appapi.Space{})
	organizations, err := client.ListOrganizationsWithResponse(ctx)
	is.NoErr(err)
	assertApplicationResponse(t, organizations.StatusCode(), organizations.Body, http.StatusOK)
	if organizations.JSON200 == nil {
		t.Fatal("organization list did not contain an array")
	}
	listed := make(map[string]bool)
	for _, item := range *organizations.JSON200 {
		listed[item.Name] = true
	}
	is.True(listed[organization] && listed[otherOrganization])
	controllersBefore := sharedApplicationControllers(t, ctx, kube)
	clusterResponse, err := client.PutClusterWithResponse(ctx, organization, cluster, appapi.ClusterInput{KubeconfigPath: &kubeconfigPath})
	is.NoErr(err)
	assertApplicationResponse(t, clusterResponse.StatusCode(), clusterResponse.Body, http.StatusOK)
	namespace := ""
	spaceResponse, err := client.PutSpaceWithResponse(ctx, organization, space, appapi.SpaceInput{Cluster: cluster})
	is.NoErr(err)
	assertApplicationResponse(t, spaceResponse.StatusCode(), spaceResponse.Body, http.StatusOK)
	if spaceResponse.JSON200 == nil {
		t.Fatal("space response did not contain a deployment target")
	}
	otherNamespace := ""
	otherSpace, err := client.PutSpaceWithResponse(ctx, organization, "staging", appapi.SpaceInput{Cluster: cluster})
	is.NoErr(err)
	assertApplicationResponse(t, otherSpace.StatusCode(), otherSpace.Body, http.StatusOK)
	if otherSpace.JSON200 == nil {
		t.Fatal("second space response did not contain a deployment target")
	}
	is.True(otherSpace.JSON200.ID != spaceResponse.JSON200.ID)
	spaces, err := client.ListSpacesWithResponse(ctx, organization)
	is.NoErr(err)
	assertApplicationResponse(t, spaces.StatusCode(), spaces.Body, http.StatusOK)
	if spaces.JSON200 == nil {
		t.Fatal("space list did not contain an array")
	}
	is.Equal(len(*spaces.JSON200), 2)
	clusters, err := client.ListClustersWithResponse(ctx, organization)
	is.NoErr(err)
	assertApplicationResponse(t, clusters.StatusCode(), clusters.Body, http.StatusOK)
	if clusters.JSON200 == nil {
		t.Fatal("cluster list did not contain an array")
	}
	is.Equal(len(*clusters.JSON200), 1)
	is.Equal((*clusters.JSON200)[0].Name, cluster)
	otherClusters, err := client.ListClustersWithResponse(ctx, otherOrganization)
	is.NoErr(err)
	assertApplicationResponse(t, otherClusters.StatusCode(), otherClusters.Body, http.StatusOK)
	if otherClusters.JSON200 == nil {
		t.Fatal("empty cluster list must be an array")
	}
	is.Equal(len(*otherClusters.JSON200), 0)
	crossOrganizationSpace, err := client.PutSpaceWithResponse(ctx, otherOrganization, "staging", appapi.SpaceInput{Cluster: cluster})
	is.NoErr(err)
	assertApplicationResponse(t, crossOrganizationSpace.StatusCode(), crossOrganizationSpace.Body, http.StatusNotFound)
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		for _, name := range []string{namespace, otherNamespace} {
			if name == "" {
				continue
			}
			if err := kube.CoreV1().Namespaces().Delete(cleanupContext, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				t.Errorf("clean up namespace %s: %v", name, err)
			}
		}
	})

	// Explicit solver zones authorize planning independently of issuer readiness.
	// The absent account key holds issuance without creating cluster credentials.
	issuerPolicy := certv1.IssuerSpec{IssuerConfig: certv1.IssuerConfig{ACME: &acmev1.ACMEIssuer{
		Server:                      "https://127.0.0.1:1/directory",
		DisableAccountKeyGeneration: true,
		PrivateKey:                  certmeta.SecretKeySelector{LocalObjectReference: certmeta.LocalObjectReference{Name: "application-test-" + suffix}},
		Solvers: []acmev1.ACMEChallengeSolver{{
			Selector: &acmev1.CertificateDNSNameSelector{DNSZones: []string{"localhost"}},
			HTTP01:   &acmev1.ACMEChallengeSolverHTTP01{Ingress: &acmev1.ACMEChallengeSolverHTTP01Ingress{}},
		}},
	}}}
	_, err = certificates.CertmanagerV1().ClusterIssuers().Create(ctx, &certv1.ClusterIssuer{
		ObjectMeta: metav1.ObjectMeta{Name: issuer}, Spec: issuerPolicy,
	}, metav1.CreateOptions{})
	is.NoErr(err)
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if err := certificates.CertmanagerV1().ClusterIssuers().Delete(cleanupContext, issuer, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("clean up test certificate issuer: %v", err)
		}
	})
	setIssuerSpec := func(spec certv1.IssuerSpec) {
		t.Helper()
		is.NoErr(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			current, err := certificates.CertmanagerV1().ClusterIssuers().Get(ctx, issuer, metav1.GetOptions{})
			if err != nil {
				return err
			}
			current.Spec = spec
			_, err = certificates.CertmanagerV1().ClusterIssuers().Update(ctx, current, metav1.UpdateOptions{})
			return err
		}))
	}

	t.Log("converting the YAML fixture locally and checking both application conversion directions")
	fixture, err := os.ReadFile("testdata/apps.yaml")
	is.NoErr(err)
	host := "borq-" + suffix + ".localhost"
	fixture = []byte(strings.NewReplacer("BORQ_TEST_IMAGE", image, "BORQ_TEST_HOST", host).Replace(string(fixture)))
	fixtureJson, err := yaml.YAMLToJSONStrict(fixture)
	is.NoErr(err)
	missingImage, err := yaml.YAMLToJSONStrict([]byte("apps:\n  - name: missing-image\n    port: 80\n"))
	is.NoErr(err)
	applicationPost(t, ctx, serverUrl+"/api/v1/apps/expand", missingImage, http.StatusBadRequest)
	invalidPlan, err := client.PostPlanWithBodyWithResponse(ctx, organization, space, "application/json", bytes.NewReader(missingImage))
	is.NoErr(err)
	assertApplicationResponse(t, invalidPlan.StatusCode(), invalidPlan.Body, http.StatusBadRequest)
	expandedBody := applicationPost(t, ctx, serverUrl+"/api/v1/apps/expand", fixtureJson, http.StatusOK)
	var expanded appapi.Configuration
	is.NoErr(json.Unmarshal(expandedBody, &expanded))
	is.Equal(len(expanded.Apps), 2)
	publishApplicationEnvironmentGroup(t, ctx, serverUrl, organization, space, "backend-config", map[string]string{"RETAIN_ME": "first", "BOOLEAN_TEXT": "true", "NUMBER_TEXT": "123", "EMPTY_TEXT": ""})
	firstResponse, err := client.PostPlanWithBodyWithResponse(ctx, organization, space, "application/json", bytes.NewReader(fixtureJson))
	is.NoErr(err)
	assertApplicationResponse(t, firstResponse.StatusCode(), firstResponse.Body, http.StatusCreated)
	if firstResponse.JSON201 == nil {
		t.Fatal("plan response did not contain saved artifacts")
	}
	firstPlan := *firstResponse.JSON201
	namespace = firstPlan.Values["namespace"].(string)
	expanded = firstPlan.Configuration
	frontend := applicationByName(t, expanded, "frontend")
	backend := applicationByName(t, expanded, "backend")
	is.Equal(backend.Env.Version, "1")
	is.Equal(len(firstPlan.Manifests), 8)
	for _, difference := range firstPlan.ResourceDiffs {
		is.Equal(difference.Action, appapi.Create)
	}
	renderInput, err := json.Marshal(struct {
		Configuration     appapi.Configuration `json:"configuration"`
		Namespace         string               `json:"namespace"`
		TargetId          string               `json:"targetID"`
		PlanId            string               `json:"planID"`
		CertificateIssuer string               `json:"certificateIssuer"`
	}{expanded, namespace, spaceResponse.JSON200.ID, firstPlan.ID, issuer})
	is.NoErr(err)
	renderedBody := applicationPost(t, ctx, serverUrl+"/api/v1/apps/render", renderInput, http.StatusOK)
	var rendered struct {
		Values          map[string]any    `json:"values"`
		Manifests       []appapi.Manifest `json:"manifests"`
		RendererVersion string            `json:"rendererVersion"`
	}
	is.NoErr(json.Unmarshal(renderedBody, &rendered))
	is.Equal(rendered.Manifests, firstPlan.Manifests)
	is.Equal(rendered.Values, firstPlan.Values)
	is.Equal(rendered.RendererVersion, firstPlan.RendererVersion)
	is.Equal(inspectApplicationManifests(t, ctx, serverUrl, rendered.Manifests), expanded)
	_, err = kube.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	is.True(apierrors.IsNotFound(err)) // Both planning and conversion are read-only.

	t.Log("applying the saved plan, inspecting live resources, and checking issued HTTPS through Traefik")
	firstArtifacts, err := json.Marshal(firstPlan.Manifests)
	is.NoErr(err)
	pendingApply, err := client.ApplyPlanWithResponse(ctx, firstPlan.ID)
	is.NoErr(err)
	assertApplicationResponse(t, pendingApply.StatusCode(), pendingApply.Body, http.StatusAccepted)
	if pendingApply.JSON202 == nil {
		t.Fatal("apply response did not contain an operation")
	}
	assertApplicationWaitingForCertificate(t, ctx, client, pendingApply.JSON202.ID)
	// Issue through the real controller without public DNS or ACME credentials.
	// Restore the explicit policy after issuance so subsequent plans revalidate it.
	setIssuerSpec(certv1.IssuerSpec{IssuerConfig: certv1.IssuerConfig{SelfSigned: &certv1.SelfSignedIssuer{}}})
	applyApplicationPlan(t, ctx, client, firstPlan.ID)
	setIssuerSpec(issuerPolicy)
	applyApplicationPlan(t, ctx, client, firstPlan.ID) // Retrying a plan does not create a revision.
	storedPlan, err := client.GetPlanWithResponse(ctx, firstPlan.ID)
	is.NoErr(err)
	assertApplicationResponse(t, storedPlan.StatusCode(), storedPlan.Body, http.StatusOK)
	storedArtifacts, err := json.Marshal(storedPlan.JSON200.Manifests)
	is.NoErr(err)
	is.Equal(string(storedArtifacts), string(firstArtifacts))
	assertLiveApplicationConfiguration(t, ctx, kube, serverUrl, namespace, expanded)
	if clusterResponse.JSON200 == nil || clusterResponse.JSON200.ID == nil {
		t.Fatal("cluster response did not contain its stable ID")
	}
	clusterID := *clusterResponse.JSON200.ID
	namespaceResource, err := kube.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	is.NoErr(err)
	assertApplicationOwnership(t, namespaceResource, firstPlan.Space, clusterID, "")
	deployments, err := kube.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{})
	is.NoErr(err)
	for _, deployment := range deployments.Items {
		assertApplicationOwnership(t, &deployment, firstPlan.Space, clusterID, deployment.Name)
	}
	pods, err := kube.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	is.NoErr(err)
	is.True(len(pods.Items) >= 2)
	for _, pod := range pods.Items {
		assertApplicationOwnership(t, &pod, firstPlan.Space, clusterID, pod.Labels["app.kubernetes.io/name"])
	}
	replicaSets, err := kube.AppsV1().ReplicaSets(namespace).List(ctx, metav1.ListOptions{})
	is.NoErr(err)
	for _, replicaSet := range replicaSets.Items {
		assertApplicationOwnership(t, &replicaSet, firstPlan.Space, clusterID, replicaSet.Labels["app.kubernetes.io/name"])
	}
	for _, name := range []string{"frontend", "backend"} {
		service, err := kube.CoreV1().Services(namespace).Get(ctx, name, metav1.GetOptions{})
		is.NoErr(err)
		assertApplicationOwnership(t, service, firstPlan.Space, clusterID, name)
		is.Equal(service.Spec.Type, corev1.ServiceTypeClusterIP)
		is.True(service.Spec.ClusterIP != "")
	}
	routes, err := gateways.GatewayV1().HTTPRoutes(namespace).List(ctx, metav1.ListOptions{})
	is.NoErr(err)
	is.Equal(len(routes.Items), 1)
	route := routes.Items[0]
	assertApplicationOwnership(t, &route, firstPlan.Space, clusterID, "frontend")
	is.Equal(route.Name, "frontend-public")
	is.Equal(route.Annotations["external-dns.kubernetes.io/hostname"], host)
	is.Equal(route.Spec.Hostnames, []gatewayv1.Hostname{gatewayv1.Hostname(host)})
	is.Equal(len(route.Spec.ParentRefs), 1)
	is.Equal(string(route.Spec.ParentRefs[0].Name), route.Name)
	is.Equal(string(*route.Spec.ParentRefs[0].SectionName), "websecure")
	listeners, err := gateways.GatewayV1().Gateways(namespace).List(ctx, metav1.ListOptions{})
	is.NoErr(err)
	is.Equal(len(listeners.Items), 1)
	gateway := listeners.Items[0]
	assertApplicationOwnership(t, &gateway, firstPlan.Space, clusterID, "frontend")
	is.Equal(gateway.Name, route.Name)
	is.Equal(string(gateway.Spec.GatewayClassName), "borq-traefik")
	is.Equal(len(gateway.Spec.Listeners), 1)
	listener := gateway.Spec.Listeners[0]
	is.Equal(listener.Protocol, gatewayv1.HTTPSProtocolType)
	is.Equal(string(*listener.Hostname), host)
	is.Equal(len(listener.TLS.CertificateRefs), 1)
	secretName := string(listener.TLS.CertificateRefs[0].Name)
	is.True(secretName != "")
	is.True(len(secretName) <= 63)
	ingresses, err := kube.NetworkingV1().Ingresses(namespace).List(ctx, metav1.ListOptions{})
	is.NoErr(err)
	is.Equal(len(ingresses.Items), 0)
	certificate, err := certificates.CertmanagerV1().Certificates(namespace).Get(ctx, secretName, metav1.GetOptions{})
	is.NoErr(err)
	assertApplicationOwnership(t, certificate, firstPlan.Space, clusterID, "frontend")
	is.Equal(certificate.Spec.SecretName, secretName)
	is.Equal(certificate.Spec.DNSNames, []string{host})
	is.Equal(certificate.Spec.IssuerRef.Name, issuer)
	is.Equal(certificate.Spec.IssuerRef.Kind, "ClusterIssuer")
	certificateReady := false
	for _, condition := range certificate.Status.Conditions {
		if condition.Type == certv1.CertificateConditionReady && condition.Status == certmeta.ConditionTrue && condition.ObservedGeneration == certificate.Generation {
			certificateReady = true
		}
	}
	is.True(certificateReady)
	secret, err := kube.CoreV1().Secrets(namespace).Get(ctx, secretName, metav1.GetOptions{})
	is.NoErr(err)
	is.Equal(secret.Type, corev1.SecretTypeTLS)
	assertApplicationOwnership(t, secret, firstPlan.Space, clusterID, "frontend")
	is.True(len(secret.Data[corev1.TLSPrivateKeyKey]) > 0)
	assertApplicationRoute(t, ctx, ingressUrl, host, secret.Data[corev1.TLSCertKey])
	secrets, err := kube.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{LabelSelector: "borq.run/space-id=" + firstPlan.Space.ID})
	is.NoErr(err)
	is.True(len(secrets.Items) >= 2) // Both the TLS Secret and pinned environment Secret carry ownership.
	for _, secret := range secrets.Items {
		assertApplicationOwnership(t, &secret, firstPlan.Space, clusterID, secret.Labels["borq.run/app-name"])
	}
	// Controller status and API defaults must not appear as an application change.
	unchanged := createApplicationPlan(t, ctx, client, organization, space, appapi.Patch{})
	is.Equal(unchanged.BaseRevision, 1)
	for _, difference := range unchanged.ResourceDiffs {
		if difference.Action != appapi.Unchanged {
			live, _ := json.Marshal(difference.Live)
			desired, _ := json.Marshal(difference.Desired)
			t.Fatalf("unexpected %s for %s %s; live=%s desired=%s", difference.Action, difference.Kind, difference.Name, live, desired)
		}
	}

	t.Log("detecting live drift and restoring all apps from a backend-only update")
	is.NoErr(retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current, err := kube.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
		if err != nil {
			return err
		}
		delete(current.Labels, "borq.run/organization-id")
		delete(current.Annotations, "borq.run/space-id")
		current.Annotations["example.com/keep"] = "retained"
		_, err = kube.CoreV1().Namespaces().Update(ctx, current, metav1.UpdateOptions{})
		return err
	}))
	is.NoErr(retry.RetryOnConflict(retry.DefaultRetry, func() error {
		deployment, err := kube.AppsV1().Deployments(namespace).Get(ctx, "frontend", metav1.GetOptions{})
		if err != nil {
			return err
		}
		deployment.Spec.Replicas = int32Pointer(2)
		delete(deployment.Labels, "borq.run/organization-id")
		delete(deployment.Annotations, "borq.run/space-id")
		deployment.Spec.Template.Spec.Containers[0].Env = append(deployment.Spec.Template.Spec.Containers[0].Env, corev1.EnvVar{Name: "MANUAL_ONLY", Value: "drift"})
		deployment.Spec.Template.Spec.Containers[0].Args = []string{"manual-argument"}
		_, err = kube.AppsV1().Deployments(namespace).Update(ctx, deployment, metav1.UpdateOptions{FieldManager: "integration-manual"})
		return err
	}))
	publishApplicationEnvironmentGroup(t, ctx, serverUrl, organization, space, "backend-config", map[string]string{"RETAIN_ME": "first", "BOOLEAN_TEXT": "true", "NUMBER_TEXT": "123", "EMPTY_TEXT": "", "CONFIG_VERSION": "two"})
	backendPatch := []appapi.AppPatch{{Name: "backend", Env: applicationEnvPatch("backend-config", "latest")}}
	updatedPlan := createApplicationPlan(t, ctx, client, organization, space, appapi.Patch{Apps: &backendPatch})
	is.Equal(len(updatedPlan.Configuration.Apps), 2)
	is.Equal(applicationByName(t, updatedPlan.Configuration, "frontend"), frontend)
	is.Equal(applicationByName(t, updatedPlan.Configuration, "backend").Env.Version, "2")
	frontendUpdate := false
	for _, difference := range updatedPlan.ResourceDiffs {
		if difference.Kind == "Deployment" && difference.Name == "frontend" {
			frontendUpdate = difference.Action == appapi.Update
		}
	}
	is.True(frontendUpdate)
	applyApplicationPlan(t, ctx, client, updatedPlan.ID)
	assertLiveApplicationConfiguration(t, ctx, kube, serverUrl, namespace, updatedPlan.Configuration)
	restoredFrontend, err := kube.AppsV1().Deployments(namespace).Get(ctx, "frontend", metav1.GetOptions{})
	is.NoErr(err)
	assertApplicationOwnership(t, restoredFrontend, firstPlan.Space, clusterID, "frontend")
	namespaceResource, err = kube.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	is.NoErr(err)
	assertApplicationOwnership(t, namespaceResource, firstPlan.Space, clusterID, "")
	is.Equal(namespaceResource.Annotations["example.com/keep"], "retained")
	is.Equal(len(restoredFrontend.Spec.Template.Spec.Containers[0].Args), 0)

	t.Log("selecting a published empty config, scaling to zero, and overriding readiness independently")
	publishApplicationEnvironmentGroup(t, ctx, serverUrl, organization, space, "empty-config", map[string]string{})
	clearPatch := []appapi.AppPatch{{Name: "backend", Env: applicationEnvPatch("empty-config", "latest")}}
	cleared := createApplicationPlan(t, ctx, client, organization, space, appapi.Patch{Apps: &clearPatch})
	is.Equal(applicationByName(t, cleared.Configuration, "backend").Env.Name, "empty-config")
	applyApplicationPlan(t, ctx, client, cleared.ID)
	assertLiveApplicationConfiguration(t, ctx, kube, serverUrl, namespace, cleared.Configuration)
	zero := 0
	scalePatch := []appapi.AppPatch{{Name: "backend", Replicas: &zero}}
	scaled := createApplicationPlan(t, ctx, client, organization, space, appapi.Patch{Apps: &scalePatch})
	applyApplicationPlan(t, ctx, client, scaled.ID)
	assertLiveApplicationConfiguration(t, ctx, kube, serverUrl, namespace, scaled.Configuration)
	one, readinessPath := 1, "/"
	restorePatch := []appapi.AppPatch{{Name: "backend", Replicas: &one, Checks: &appapi.ChecksPatch{Readiness: &appapi.CheckPatch{Path: &readinessPath}}}}
	restored := createApplicationPlan(t, ctx, client, organization, space, appapi.Patch{Apps: &restorePatch})
	is.Equal(applicationByName(t, restored.Configuration, "backend").Checks.Readiness.Path, "/")
	is.Equal(applicationByName(t, restored.Configuration, "backend").Checks.Startup.Path, "/healthz")
	applyApplicationPlan(t, ctx, client, restored.ID)
	assertLiveApplicationConfiguration(t, ctx, kube, serverUrl, namespace, restored.Configuration)
	assertApplicationRoute(t, ctx, ingressUrl, host, secret.Data[corev1.TLSCertKey])
	spaceState, err := client.GetSpaceWithResponse(ctx, organization, space)
	is.NoErr(err)
	assertApplicationResponse(t, spaceState.StatusCode(), spaceState.Body, http.StatusOK)
	is.Equal(spaceState.JSON200.Revision, 5)
	is.Equal(spaceState.JSON200.LastSuccessfulRevision, 5)
	is.Equal(spaceState.JSON200.Configuration, restored.Configuration)
	staging, err := client.GetSpaceWithResponse(ctx, organization, "staging")
	is.NoErr(err)
	assertApplicationResponse(t, staging.StatusCode(), staging.Body, http.StatusOK)
	is.Equal(staging.JSON200.Revision, 0)
	is.Equal(len(staging.JSON200.Configuration.Apps), 0)
	// The same app and config names are isolated by the space's namespace and target ID.
	publishApplicationEnvironmentGroup(t, ctx, serverUrl, organization, "staging", "backend-config", map[string]string{"SPACE": "staging"})
	stagingConfig, err := client.GetEnvironmentGroupWithResponse(ctx, organization, "staging", "backend-config")
	is.NoErr(err)
	assertApplicationResponse(t, stagingConfig.StatusCode(), stagingConfig.Body, http.StatusOK)
	assertApplicationVariables(t, stagingConfig.JSON200.Values, map[string]string{"SPACE": "staging"})
	productionConfig, err := client.GetEnvironmentGroupWithResponse(ctx, organization, space, "backend-config")
	is.NoErr(err)
	assertApplicationResponse(t, productionConfig.StatusCode(), productionConfig.Body, http.StatusOK)
	is.Equal(productionConfig.JSON200.Version, 2)
	assertApplicationVariables(t, productionConfig.JSON200.Values, map[string]string{"RETAIN_ME": "first", "BOOLEAN_TEXT": "true", "NUMBER_TEXT": "123", "EMPTY_TEXT": "", "CONFIG_VERSION": "two"})
	stagingApps := []appapi.AppPatch{{Name: "frontend", Image: &image, Replicas: &zero, Env: applicationEnvPatch("backend-config", "latest")}}
	stagingPlan, err := client.PostPlanWithResponse(ctx, organization, "staging", appapi.Patch{Apps: &stagingApps})
	is.NoErr(err)
	assertApplicationResponse(t, stagingPlan.StatusCode(), stagingPlan.Body, http.StatusCreated)
	applyApplicationPlan(t, ctx, client, stagingPlan.JSON201.ID)
	otherNamespace = stagingPlan.JSON201.Values["namespace"].(string)
	is.True(otherNamespace != namespace)
	assertLiveApplicationConfiguration(t, ctx, kube, serverUrl, otherNamespace, stagingPlan.JSON201.Configuration)
	assertLiveApplicationConfiguration(t, ctx, kube, serverUrl, namespace, restored.Configuration)
	is.Equal(sharedApplicationControllers(t, ctx, kube), controllersBefore)

	t.Run("deletion", func(t *testing.T) {
		assert := is.New(t)
		staging, err := kube.AppsV1().Deployments(otherNamespace).Get(ctx, "frontend", metav1.GetOptions{})
		assert.NoErr(err)
		for _, name := range []string{"frontend", "backend"} {
			removed, err := client.DeleteAppWithResponse(ctx, organization, space, name)
			assert.NoErr(err)
			assertApplicationResponse(t, removed.StatusCode(), removed.Body, http.StatusAccepted)
			assert.Equal(string(removed.JSON202.Phase), "deleting")
			waitPredeployOperation(t, ctx, client, removed.JSON202.ID, appapi.Succeeded)
			_, err = kube.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
			assert.True(apierrors.IsNotFound(err))
			_, err = kube.CoreV1().Services(namespace).Get(ctx, name, metav1.GetOptions{})
			assert.True(apierrors.IsNotFound(err))
			pods, err := kube.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/name=" + name})
			assert.NoErr(err)
			assert.Equal(len(pods.Items), 0)
			_, err = gateways.GatewayV1().HTTPRoutes(namespace).Get(ctx, "frontend-public", metav1.GetOptions{})
			assert.True(apierrors.IsNotFound(err))
			_, err = gateways.GatewayV1().Gateways(namespace).Get(ctx, "frontend-public", metav1.GetOptions{})
			assert.True(apierrors.IsNotFound(err))
			_, err = certificates.CertmanagerV1().Certificates(namespace).Get(ctx, secretName, metav1.GetOptions{})
			assert.True(apierrors.IsNotFound(err))
			missing, err := client.GetAppWithResponse(ctx, organization, space, name)
			assert.NoErr(err)
			assertApplicationResponse(t, missing.StatusCode(), missing.Body, http.StatusNotFound)
		}
		state, err := client.ListAppsWithResponse(ctx, organization, space)
		assert.NoErr(err)
		assert.Equal(len(state.JSON200.Configuration.Apps), 0)
		assert.Equal(len(state.JSON200.Manifests), 0)
		projections, err := kube.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/name=backend"})
		assert.NoErr(err)
		assert.Equal(len(projections.Items), 0)
		_, err = kube.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
		assert.NoErr(err)
		unchanged, err := kube.AppsV1().Deployments(otherNamespace).Get(ctx, "frontend", metav1.GetOptions{})
		assert.NoErr(err)
		assert.Equal(unchanged.UID, staging.UID)
		group, err := client.GetEnvironmentGroupWithResponse(ctx, organization, space, "backend-config")
		assert.NoErr(err)
		assertApplicationResponse(t, group.StatusCode(), group.Body, http.StatusOK)
		assert.Equal(sharedApplicationControllers(t, ctx, kube), controllersBefore)

		// A stopped SQLite app still owns durable storage and initialization state.
		t.Log("deleting a stopped SQLite app while preserving its volume and recovery state")
		enabled, disabled := true, false
		retainedApps := []appapi.AppPatch{{Name: "retained", Image: &image, Replicas: &zero, Sqlite: &appapi.SqlitePatch{Enabled: &enabled, Backups: &disabled}}}
		retainedPlan := createApplicationPlan(t, ctx, client, organization, space, appapi.Patch{Apps: &retainedApps})
		applyApplicationPlan(t, ctx, client, retainedPlan.ID)
		claim, err := kube.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, "retained-sqlite", metav1.GetOptions{})
		assert.NoErr(err)
		recovery, err := kube.CoreV1().ConfigMaps(namespace).Get(ctx, "retained-sqlite-state", metav1.GetOptions{})
		assert.NoErr(err)
		removed, err := client.DeleteAppWithResponse(ctx, organization, space, "retained")
		assert.NoErr(err)
		assertApplicationResponse(t, removed.StatusCode(), removed.Body, http.StatusAccepted)
		waitPredeployOperation(t, ctx, client, removed.JSON202.ID, appapi.Succeeded)
		preservedClaim, err := kube.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, claim.Name, metav1.GetOptions{})
		assert.NoErr(err)
		assert.Equal(preservedClaim.UID, claim.UID)
		preservedRecovery, err := kube.CoreV1().ConfigMaps(namespace).Get(ctx, recovery.Name, metav1.GetOptions{})
		assert.NoErr(err)
		assert.Equal(preservedRecovery.UID, recovery.UID)
		assert.Equal(preservedRecovery.Data, recovery.Data)
		// Retained resources must not block subsequent plans for this space.
		_ = createApplicationPlan(t, ctx, client, organization, space, appapi.Patch{})
	})
}

// assertApplicationOwnership checks actual Kubernetes metadata against API IDs;
// metav1.Object is the metadata interface implemented by SDK resource pointers.
func assertApplicationOwnership(t *testing.T, resource metav1.Object, space appapi.Space, clusterID, appName string) {
	t.Helper()
	assert := is.New(t)
	expected := map[string]string{
		"borq.run/organization-id": space.Organization,
		"borq.run/organization":    space.Organization,
		"borq.run/space-id":        space.ID,
		"borq.run/space":           space.Name,
		"borq.run/cluster-id":      clusterID,
		"borq.run/cluster":         space.Cluster,
	}
	if appName != "" {
		expected["borq.run/app-name"] = appName
		assert.True(resource.GetLabels()["borq.run/app-id"] != "")
		expected["borq.run/app-id"] = resource.GetLabels()["borq.run/app-id"]
	}
	for key, value := range expected {
		if resource.GetLabels()[key] != value || resource.GetAnnotations()[key] != value {
			t.Fatalf("%s ownership %s: labels=%q annotations=%q want=%q", resource.GetName(), key, resource.GetLabels()[key], resource.GetAnnotations()[key], value)
		}
	}
}

// sharedApplicationControllers records shared controller identities so adding spaces
// cannot accidentally install another copy of the cluster's operators or ingress.
func sharedApplicationControllers(t *testing.T, ctx context.Context, kube *kubernetes.Clientset) map[string]string {
	t.Helper()
	deployments, err := kube.AppsV1().Deployments("").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	controllers := make(map[string]string)
	for _, deployment := range deployments.Items {
		name := deployment.Labels["app.kubernetes.io/name"]
		if strings.HasPrefix(name, "cert-manager") || strings.HasPrefix(name, "external-secrets") || name == "traefik" || name == "alloy" || name == "snapshot-controller" {
			controllers[deployment.Namespace+"/"+deployment.Name] = string(deployment.UID)
		}
	}
	if len(controllers) == 0 {
		t.Fatal("deployment fixture has no shared controllers")
	}
	return controllers
}

func applicationByName(t *testing.T, configuration appapi.Configuration, name string) appapi.App {
	t.Helper()
	for _, app := range configuration.Apps {
		if app.Name == name {
			return app
		}
	}
	t.Fatalf("expanded configuration is missing app %s", name)
	return appapi.App{}
}

func assertApplicationVariables(t *testing.T, variables []appapi.Variable, expected map[string]string) {
	t.Helper()
	actual := make(map[string]string, len(variables))
	for _, variable := range variables {
		actual[variable.Name] = variable.Value
	}
	is.New(t).Equal(actual, expected)
}

func applicationPost(t *testing.T, ctx context.Context, url string, body []byte, status int) []byte {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	authenticateApplicationTestRequest(t, request)
	response, err := applicationTestHttpClient().Do(request)
	if err != nil {
		t.Fatalf("request %s: %v", url, err)
	}
	defer response.Body.Close()
	result, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	assertApplicationResponse(t, response.StatusCode, result, status)
	if response.StatusCode == http.StatusOK && !strings.HasPrefix(response.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("conversion returned content type %s, expected application/json", response.Header.Get("Content-Type"))
	}
	return result
}

func inspectApplicationManifests(t *testing.T, ctx context.Context, serverUrl string, manifests []appapi.Manifest) appapi.Configuration {
	t.Helper()
	body, err := json.Marshal(struct {
		Manifests []appapi.Manifest `json:"manifests"`
	}{manifests})
	if err != nil {
		t.Fatal(err)
	}
	result := applicationPost(t, ctx, serverUrl+"/api/v1/apps/inspect", body, http.StatusOK)
	var configuration appapi.Configuration
	if err := json.Unmarshal(result, &configuration); err != nil {
		t.Fatal(err)
	}
	return configuration
}

// Kubernetes clients retain connection state through SDK pointer receivers.
func liveApplicationManifests(t *testing.T, ctx context.Context, kube *kubernetes.Clientset, namespace string) []appapi.Manifest {
	t.Helper()
	is := is.New(t)
	manifests := []appapi.Manifest{}
	deployments, err := kube.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{})
	is.NoErr(err)
	secretNames := map[string]bool{}
	for _, deployment := range deployments.Items {
		deployment.TypeMeta = metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"}
		manifests = append(manifests, applicationManifest(t, deployment))
		for _, container := range deployment.Spec.Template.Spec.Containers {
			for _, source := range container.EnvFrom {
				if source.SecretRef != nil {
					secretNames[source.SecretRef.Name] = true
				}
			}
		}
	}
	services, err := kube.CoreV1().Services(namespace).List(ctx, metav1.ListOptions{})
	is.NoErr(err)
	for _, service := range services.Items {
		service.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Service"}
		manifests = append(manifests, applicationManifest(t, service))
	}
	for _, resource := range []string{
		"gateway.networking.k8s.io/v1/namespaces/" + namespace + "/gateways",
		"gateway.networking.k8s.io/v1/namespaces/" + namespace + "/httproutes",
		"cert-manager.io/v1/namespaces/" + namespace + "/certificates",
	} {
		body, err := kube.CoreV1().RESTClient().Get().AbsPath("/apis/" + resource).Do(ctx).Raw()
		is.NoErr(err)
		var resources struct {
			Items []appapi.Manifest `json:"items"`
		}
		is.NoErr(json.Unmarshal(body, &resources))
		manifests = append(manifests, resources.Items...)
	}
	if len(secretNames) > 0 {
		body, err := kube.CoreV1().RESTClient().Get().AbsPath("/apis/external-secrets.io/v1/namespaces/" + namespace + "/externalsecrets").Do(ctx).Raw()
		is.NoErr(err)
		var externalSecrets struct {
			Items []appapi.Manifest `json:"items"`
		}
		is.NoErr(json.Unmarshal(body, &externalSecrets))
		for _, externalSecret := range externalSecrets.Items {
			specification, _ := externalSecret["spec"].(map[string]any)
			target, _ := specification["target"].(map[string]any)
			name, _ := target["name"].(string)
			if secretNames[name] {
				manifests = append(manifests, externalSecret)
			}
		}
	}
	return manifests
}

// Kubernetes clients retain connection state through SDK pointer receivers.
// Resolve variables from the actual generated Secret, including present empty values.
func applicationEnvironment(t *testing.T, ctx context.Context, kube *kubernetes.Clientset, namespace string, container corev1.Container) map[string]string {
	t.Helper()
	values := map[string]string{}
	for _, source := range container.EnvFrom {
		if source.SecretRef == nil {
			t.Fatal("envFrom must reference a Secret")
		}
		secret, err := kube.CoreV1().Secrets(namespace).Get(ctx, source.SecretRef.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for key, value := range secret.Data {
			values[key] = string(value)
		}
	}
	secrets := map[string]corev1.Secret{}
	for _, variable := range container.Env {
		if variable.ValueFrom == nil {
			values[variable.Name] = variable.Value
			continue
		}
		if variable.ValueFrom.SecretKeyRef == nil {
			t.Fatalf("variable %s does not reference a Borq-managed Secret", variable.Name)
		}
		reference := variable.ValueFrom.SecretKeyRef
		secret, exists := secrets[reference.Name]
		if !exists {
			read, err := kube.CoreV1().Secrets(namespace).Get(ctx, reference.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			secret = *read
			secrets[reference.Name] = secret
		}
		value, exists := secret.Data[reference.Key]
		if !exists {
			t.Fatalf("generated Secret %s is missing key %s", reference.Name, reference.Key)
		}
		values[variable.Name] = string(value)
	}
	return values
}

func applicationManifest(t *testing.T, resource any) appapi.Manifest {
	t.Helper()
	body, err := json.Marshal(resource)
	if err != nil {
		t.Fatal(err)
	}
	var manifest appapi.Manifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest
}

// Kubernetes clients retain connection state through SDK pointer receivers.
func assertLiveApplicationConfiguration(t *testing.T, ctx context.Context, kube *kubernetes.Clientset, serverUrl, namespace string, expected appapi.Configuration) {
	t.Helper()
	actual := inspectApplicationManifests(t, ctx, serverUrl, liveApplicationManifests(t, ctx, kube, namespace))
	is.New(t).Equal(actual, expected)
}

func assertApplicationRoute(t *testing.T, ctx context.Context, ingressUrl, host string, certificate []byte) {
	t.Helper()
	requestContext, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if !strings.HasPrefix(ingressUrl, "https://") {
		t.Fatal("BORQ_APPLICATION_TEST_INGRESS_URL must be an HTTPS endpoint")
	}
	trustedCertificates := x509.NewCertPool()
	if !trustedCertificates.AppendCertsFromPEM(certificate) {
		t.Fatal("cert-manager did not provide a valid certificate for the HTTPS route")
	}
	// TLS configuration uses a pointer because net/http retains transport state.
	httpsTransport := &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS12, RootCAs: trustedCertificates, ServerName: host,
	}}
	defer httpsTransport.CloseIdleConnections()
	transport := http.Client{Timeout: 5 * time.Second, Transport: httpsTransport}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	lastResult := "no response"
	for {
		request, err := http.NewRequestWithContext(requestContext, http.MethodGet, strings.TrimRight(ingressUrl, "/")+"/", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Host = host
		response, err := transport.Do(request)
		if err != nil {
			lastResult = err.Error()
		} else {
			body, readErr := io.ReadAll(response.Body)
			response.Body.Close()
			if readErr == nil && response.StatusCode == http.StatusOK && string(body) == "borq frontend\n" {
				return
			}
			lastResult = fmt.Sprintf("status=%d body=%q read=%v", response.StatusCode, body, readErr)
		}
		select {
		case <-requestContext.Done():
			t.Fatalf("Traefik route for %s did not become ready: %s", host, lastResult)
		case <-ticker.C:
		}
	}
}

// The generated client retains its HTTP transport through pointer receivers.
func assertApplicationWaitingForCertificate(t *testing.T, ctx context.Context, client *appapi.ClientWithResponses, operationId string) {
	t.Helper()
	deadline, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		response, err := client.GetOperationWithResponse(deadline, operationId)
		if err != nil {
			t.Fatal(err)
		}
		assertApplicationResponse(t, response.StatusCode(), response.Body, http.StatusOK)
		if response.JSON200 == nil {
			t.Fatal("operation response did not contain progress")
		}
		operation := *response.JSON200
		if operation.Status == appapi.Succeeded || operation.Status == appapi.Failed {
			t.Fatalf("rollout must wait for certificate issuance, got %s: %s", operation.Status, operation.Message)
		}
		for _, resource := range operation.Resources {
			if resource.Kind == "Certificate" && resource.State == "running" && resource.Message == "waiting for the certificate controller to mark this revision ready" {
				t.Logf("rollout waits on %s/%s: %s", resource.Kind, resource.Name, resource.Message)
				return
			}
		}
		select {
		case <-deadline.Done():
			t.Fatalf("rollout did not report pending certificate issuance: %s", operation.Message)
		case <-ticker.C:
		}
	}
}

// Generated clients use pointer receivers to preserve their configured HTTP transport.
func createApplicationPlan(t *testing.T, ctx context.Context, client *appapi.ClientWithResponses, organization, space string, patch appapi.Patch) appapi.Plan {
	t.Helper()
	response, err := client.PostPlanWithResponse(ctx, organization, space, patch)
	if err != nil {
		t.Fatalf("create application plan: %v", err)
	}
	assertApplicationResponse(t, response.StatusCode(), response.Body, http.StatusCreated)
	if response.JSON201 == nil {
		t.Fatal("plan response did not contain a saved plan")
	}
	return *response.JSON201
}

// Generated clients use pointer receivers to preserve their configured HTTP transport.
func applyApplicationPlan(t *testing.T, ctx context.Context, client *appapi.ClientWithResponses, planId string) {
	t.Helper()
	response, err := client.ApplyPlanWithResponse(ctx, planId)
	if err != nil {
		t.Fatalf("apply saved plan: %v", err)
	}
	assertApplicationResponse(t, response.StatusCode(), response.Body, http.StatusAccepted)
	if response.JSON202 == nil {
		t.Fatal("apply response did not contain an operation")
	}
	operationId := response.JSON202.ID
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		operation, err := client.GetOperationWithResponse(ctx, operationId)
		if err != nil {
			t.Fatalf("poll application operation: %v", err)
		}
		assertApplicationResponse(t, operation.StatusCode(), operation.Body, http.StatusOK)
		if operation.JSON200 == nil {
			t.Fatal("operation response did not contain progress")
		}
		switch operation.JSON200.Status {
		case appapi.Succeeded:
			return
		case appapi.Failed:
			t.Fatalf("application operation failed: %s", operation.JSON200.Message)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("application rollout did not finish: %s", operation.JSON200.Message)
		case <-ticker.C:
		}
	}
}

func assertApplicationResponse(t *testing.T, actual int, body []byte, expected int) {
	t.Helper()
	if actual != expected {
		t.Fatalf("application API returned %d, expected %d: %s", actual, expected, body)
	}
}

// Kubernetes replica pointers distinguish omitted values from an explicit zero.
func int32Pointer(value int32) *int32 {
	return &value
}

// Generated clients retain their configured connection through pointer receivers.
func publishApplicationEnvironmentGroup(t *testing.T, ctx context.Context, serverUrl, organization, space, name string, values map[string]string) appapi.EnvironmentGroup {
	t.Helper()
	client, err := appapi.NewClientWithResponses(serverUrl, appapi.WithHTTPClient(applicationTestHttpClient()), appapi.WithRequestEditorFn(func(_ context.Context, request *http.Request) error {
		authenticateApplicationTestRequest(t, request)
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	variables := make([]appapi.Variable, 0, len(values))
	for name, value := range values {
		variables = append(variables, appapi.Variable{Name: name, Value: value})
	}
	response, err := client.PublishEnvironmentGroupWithResponse(ctx, organization, space, name, appapi.PublishEnvironmentGroupInput{Values: variables})
	if err != nil {
		t.Fatal(err)
	}
	assertApplicationResponse(t, response.StatusCode(), response.Body, http.StatusCreated)
	if response.JSON201 == nil {
		t.Fatal("config publish returned no configuration")
	}
	return *response.JSON201
}

// Pointer fields preserve omitted patch fields in the generated OpenAPI client.
func applicationEnvPatch(name, version string) *appapi.EnvReferencePatch {
	return &appapi.EnvReferencePatch{Name: &name, Version: &version}
}

// Remote acceptance uses a real administrator credential only for the selected
// Borq origin. Local lifecycle fixtures retain their isolated raw API handler.
func authenticateApplicationTestRequest(t *testing.T, request *http.Request) {
	t.Helper()
	selected := os.Getenv("BORQ_INTEGRATION_SERVER_URL")
	if selected == "" {
		return
	}
	endpoint, err := url.Parse(selected)
	if err != nil || endpoint.User != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.RawQuery != "" || endpoint.Fragment != "" || (endpoint.Path != "" && endpoint.Path != "/") {
		t.Fatal("BORQ_INTEGRATION_SERVER_URL must be an HTTP or HTTPS server origin")
	}
	if request.URL.Scheme != endpoint.Scheme || request.URL.Host != endpoint.Host {
		return
	}
	path := os.Getenv("BORQ_APPLICATION_ADMIN_TOKEN_FILE")
	if path == "" {
		t.Fatal("remote acceptance requires BORQ_APPLICATION_ADMIN_TOKEN_FILE pointing to the private bootstrap administrator token file, plus a ready managed Pocket ID instance for organization onboarding")
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal("could not open BORQ_APPLICATION_ADMIN_TOKEN_FILE; use the private administrator token file printed by bootstrap")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		t.Fatal("BORQ_APPLICATION_ADMIN_TOKEN_FILE must be a regular owner-only file; run chmod 600 on the token file")
	}
	encoded, err := io.ReadAll(io.LimitReader(file, 4099))
	token := strings.TrimSpace(string(encoded))
	if err != nil || len(token) < 32 || len(token) > 4096 || strings.ContainsAny(token, " \t\r\n") {
		t.Fatal("BORQ_APPLICATION_ADMIN_TOKEN_FILE must contain a valid administrator token")
	}
	request.Header.Set("Authorization", "Bearer "+token)
}

func applicationTestHttpClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
}
