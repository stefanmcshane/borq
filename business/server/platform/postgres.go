package platform

import (
	"context"
	"fmt"
	"strings"

	"github.com/stefanmcshane/borq/foundations/helm"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	"go.opentelemetry.io/otel"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeclient "k8s.io/client-go/kubernetes"
)

// ReconcilePostgresComponents installs managed PostgreSQL controllers on demand using the component catalog.
func ReconcilePostgresComponents(ctx context.Context, input TargetInput) (Result, error) {
	settings, err := normalizeSettings(input.Settings)
	if err != nil {
		return Result{}, err
	}
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: input.KubeconfigPath, Context: input.Context, InCluster: input.InCluster}, logger.Logger{})
	if err != nil {
		return Result{}, err
	}
	releases, err := helm.NewReleaseManager(helm.NewReleaseManagerInput{Connection: connection, KubeconfigPath: input.KubeconfigPath})
	if err != nil {
		return Result{}, err
	}
	defer releases.Close()
	definitions := postgresComponents(settings)
	if err := checkPostgresControllers(ctx, connection, releases, settings.Namespace, definitions); err != nil {
		return Result{}, err
	}
	if err := prepareNamespace(ctx, connection, settings.Namespace); err != nil {
		return Result{}, err
	}
	return reconcileComponents(ctx, componentRun{connection: connection, kubeconfigPath: input.KubeconfigPath, scope: "postgres", values: valuesInput{Settings: settings}, definitions: definitions, upgrade: true, upgradeComponents: input.UpgradeComponents, recoverPending: input.RecoverPending})
}

// ReadPostgresComponents reads only known installed controller releases from the
// selected target. It preserves partial observations without changing resources.
func ReadPostgresComponents(ctx context.Context, input TargetInput) (Result, error) {
	ctx, span := otel.Tracer("borq/platform").Start(ctx, "platform.read_postgres_components")
	defer span.End()
	catalog, err := GetComponentCatalog(ComponentCatalogInput{Scope: ComponentScopePostgres, Settings: input.Settings})
	if err != nil {
		return Result{}, err
	}
	result := Result{Profile: catalog.Profile, Namespace: catalog.Namespace, Components: []ComponentState{}}
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: input.KubeconfigPath, Context: input.Context, InCluster: input.InCluster}, logger.Logger{})
	if err != nil {
		return result, err
	}
	releases, err := helm.NewReleaseManager(helm.NewReleaseManagerInput{Connection: connection, KubeconfigPath: input.KubeconfigPath})
	if err != nil {
		return result, err
	}
	defer releases.Close()
	var inspectionError error
	for _, definition := range catalog.Components {
		installed, err := releases.Inspect(ctx, helm.InstallOrUpgradeInput{Namespace: catalog.Namespace, ReleaseName: definition.Name, ChartRef: definition.Chart, RepositoryUrl: definition.Repository, Version: definition.Version, Values: definition.Values, PodLabels: componentPodLabels()})
		if err != nil {
			return result, err
		}
		if installed.Status == "absent" || installed.Status == "uninstalled" {
			continue
		}
		if installed.Chart != definition.Chart {
			inspectionError = fmt.Errorf("PostgreSQL component %s has unexpected installed chart", definition.Name)
			continue
		}
		result.Components = append(result.Components, ComponentState{Name: definition.Name, Chart: installed.Chart, Version: installed.Version, Revision: installed.Revision, Status: installed.Status, UpgradeAvailable: installed.UpgradeAvailable})
	}
	return result, inspectionError
}

func postgresComponents(settings Settings) []component {
	return overrideComponents([]component{
		{Name: "cloudnative-pg", Description: "Runs isolated managed PostgreSQL databases.", Repository: "https://cloudnative-pg.github.io/charts", Chart: "cloudnative-pg", Version: "0.29.1", Values: "cloudnative-pg.yaml", InstallOrder: 10, UpgradeOrder: 10},
		{Name: "plugin-barman-cloud", Description: "Archives PostgreSQL backups and WAL to shared S3.", Repository: "https://cloudnative-pg.github.io/charts", Chart: "plugin-barman-cloud", Version: "0.8.1", Values: "plugin-barman-cloud.yaml", InstallOrder: 20, UpgradeOrder: 20, DependsOn: []string{"cloudnative-pg"}},
	}, settings.Components)
}

func checkPostgresControllers(ctx context.Context, connection kubernetes.Connection, releases helm.ReleaseManager, namespace string, definitions []component) error {
	installed, err := releases.List(ctx, "")
	if err != nil {
		return err
	}
	for _, release := range installed {
		for _, definition := range definitions {
			if release.State.Chart != definition.Chart && release.State.Chart != "cloudnative-pg" && release.State.Chart != "plugin-barman-cloud" {
				continue
			}
			if release.Namespace == namespace && release.State.Name == definition.Name && release.State.Chart == definition.Chart {
				continue
			}
			if release.State.Chart == definition.Chart {
				return fmt.Errorf("existing PostgreSQL controller release %s/%s must be explicitly migrated before managed addons can install", release.Namespace, release.State.Name)
			}
		}
	}
	configuration := connection.RestConfig()
	client, err := kubeclient.NewForConfig(&configuration)
	if err != nil {
		return err
	}
	deployments, err := client.AppsV1().Deployments("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, deployment := range deployments.Items {
		postgres := false
		for _, container := range deployment.Spec.Template.Spec.Containers {
			image := strings.ToLower(container.Image)
			if strings.Contains(image, "cloudnative-pg/cloudnative-pg:") || strings.Contains(image, "plugin-barman-cloud:") {
				postgres = true
			}
		}
		if !postgres {
			continue
		}
		release := deployment.Annotations["meta.helm.sh/release-name"]
		owned := false
		for _, definition := range definitions {
			if deployment.Namespace == namespace && release == definition.Name {
				owned = true
			}
		}
		if !owned {
			return fmt.Errorf("existing PostgreSQL controller Deployment %s/%s requires explicit migration", deployment.Namespace, deployment.Name)
		}
	}
	return nil
}
