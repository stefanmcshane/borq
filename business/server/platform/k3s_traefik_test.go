package platform

import (
	"context"
	"strings"
	"testing"

	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/foundations/helm"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	releasev1 "helm.sh/helm/v4/pkg/release/v1"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestTraefikCoexistencePreflight reproduces the two-release upgrade blocker and
// rejects a same-named foreign release without mutating either installation.
func TestTraefikCoexistencePreflight(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(map[bool]string{false: "owned legacy", true: "foreign legacy"}[foreign], func(t *testing.T) {
			is := is.New(t)
			definition := sharedCatalog()[1]
			legacy := diffTestRelease(t, definition, diffTestInput())
			legacy.Name = "traefik"
			if foreign {
				legacy.Labels = nil
			}
			bundled := legacy
			bundled.Namespace = "kube-system"
			bundled.Labels = nil
			path, requests := diffTestServer(t, diffTestInventory{releases: []releasev1.Release{legacy, bundled}, deployments: []appsv1.Deployment{
				{ObjectMeta: metav1.ObjectMeta{Name: "traefik", Namespace: SystemNamespace, Labels: map[string]string{"app.kubernetes.io/name": "traefik"}, Annotations: map[string]string{"meta.helm.sh/release-name": "traefik", "meta.helm.sh/release-namespace": SystemNamespace}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "traefik", Namespace: "kube-system", Labels: map[string]string{"app.kubernetes.io/name": "traefik"}}},
			}})
			connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: path, Context: "selected"}, logger.Logger{})
			is.NoErr(err)
			releases, err := helm.NewReleaseManager(helm.NewReleaseManagerInput{Connection: connection, KubeconfigPath: path})
			is.NoErr(err)
			defer releases.Close()
			conflicts, err := targetComponentConflicts(context.Background(), connection, releases, SystemNamespace, sharedCatalog())
			is.NoErr(err)
			if foreign {
				is.True(strings.Contains(conflicts["traefik"], "not installed by Borq"))
			} else {
				is.Equal(conflicts["traefik"], "")
			}
			for len(requests) > 0 {
				is.True(strings.HasPrefix(<-requests, "GET "))
			}
		})
	}
}
