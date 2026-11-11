package management

import (
	"strings"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

// TestBootstrapPreservesInstalledClusterName keeps public addresses and storage
// naming stable on reruns, including retained configuration after teardown.
func TestBootstrapPreservesInstalledClusterName(t *testing.T) {
	for _, test := range []struct {
		name, deploymentName, storedName, explicitName, expected, failure string
		legacyMetadata                                                    bool
		foreign                                                           bool
	}{
		{name: "fresh", expected: "existing-cluster"},
		{name: "retained legacy metadata", legacyMetadata: true, failure: "existing --cluster-name"},
		{name: "explicit legacy recovery", legacyMetadata: true, explicitName: "production", expected: "production"},
		{name: "existing RC10", deploymentName: "production", expected: "production"},
		{name: "retained networking", storedName: "production", expected: "production"},
		{name: "matching records", deploymentName: "production", storedName: "production", expected: "production"},
		{name: "explicit matching", deploymentName: "production", explicitName: "production", expected: "production"},
		{name: "explicit rename", deploymentName: "production", explicitName: "replacement", failure: "renaming requires"},
		{name: "inconsistent records", deploymentName: "production", storedName: "other", failure: "inconsistent"},
		{name: "foreign deployment", deploymentName: "production", foreign: true, failure: "owned outside Borq"},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			request := Request{ClusterName: "existing-cluster", KubeconfigPath: "/selected/kubeconfig", inferredClusterName: true}.withDefaults()
			if test.explicitName != "" {
				request.ClusterName, request.inferredClusterName = test.explicitName, false
			}
			var objects []runtime.Object
			if test.deploymentName != "" {
				labels := appLabels(borqName, "server")
				if test.foreign {
					labels = nil
				}
				deployment := borqDeployment(request)
				deployment.Labels = labels
				deployment.Spec.Template.Spec.Containers[0].Args = []string{"server", "--management-cluster-name=" + test.deploymentName}
				objects = append(objects, deployment)
			}
			if test.storedName != "" || test.legacyMetadata {
				objects = append(objects, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "borq-network-settings", Namespace: request.Namespace, Labels: appLabels("borq-network-settings", "network")}, Data: map[string]string{"clusterName": test.storedName}})
			}
			client := fake.NewClientset(objects...)
			resolved, err := restoreManagementClusterName(t.Context(), kubernetes.NewConnectionForTest(client), request)
			if test.failure != "" {
				assert.True(err != nil)
				assert.True(strings.Contains(err.Error(), test.failure))
			} else {
				assert.NoErr(err)
				assert.Equal(resolved.ClusterName, test.expected)
			}
			for _, action := range client.Actions() {
				assert.Equal(action.GetVerb(), "get")
			}
		})
	}
}
