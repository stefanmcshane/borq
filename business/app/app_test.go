package app

import (
	"context"
	"errors"
	"testing"

	"github.com/matryer/is"

	"github.com/stefanmcshane/borq/foundations/kubernetes"
)

func TestApplyOrdersServicesBeforeDeployment(t *testing.T) {
	is := is.New(t)

	operations := []string{}
	managedApp := App{
		Namespace: "borq-system",
		Services: []kubernetes.Service{
			{Name: "first", Namespace: "borq-system"},
			{Name: "second", Namespace: "borq-system"},
		},
		Workload: Workload{
			Kind: DeploymentWorkload,
			Deployment: kubernetes.Deployment{
				Name:      "whoami",
				Namespace: "borq-system",
			},
		},
	}

	err := managedApp.apply(context.Background(), applyFuncs{
		service: func(_ context.Context, service kubernetes.Service) error {
			operations = append(operations, "service:"+service.Name)
			return nil
		},
		deployment: func(_ context.Context, deployment kubernetes.Deployment) error {
			operations = append(operations, "deployment:"+deployment.Name)
			return nil
		},
	})

	is.NoErr(err)
	is.Equal(operations, []string{"service:first", "service:second", "deployment:whoami"})
}

func TestApplyReturnsServiceError(t *testing.T) {
	is := is.New(t)

	wantErr := errors.New("service apply failed")
	managedApp := App{
		Namespace: "borq-system",
		Services: []kubernetes.Service{
			{Name: "whoami", Namespace: "borq-system"},
		},
		Workload: Workload{
			Kind: DeploymentWorkload,
			Deployment: kubernetes.Deployment{
				Name:      "whoami",
				Namespace: "borq-system",
			},
		},
	}

	err := managedApp.apply(context.Background(), applyFuncs{
		service: func(context.Context, kubernetes.Service) error {
			return wantErr
		},
		deployment: func(context.Context, kubernetes.Deployment) error { return nil },
	})

	is.Equal(err, wantErr)
}

func TestApplyReturnsDeploymentError(t *testing.T) {
	is := is.New(t)

	wantErr := errors.New("deployment apply failed")
	managedApp := App{
		Namespace: "borq-system",
		Workload: Workload{
			Kind: DeploymentWorkload,
			Deployment: kubernetes.Deployment{
				Name:      "whoami",
				Namespace: "borq-system",
			},
		},
	}

	err := managedApp.apply(context.Background(), applyFuncs{
		service: func(context.Context, kubernetes.Service) error { return nil },
		deployment: func(context.Context, kubernetes.Deployment) error {
			return wantErr
		},
		statefulSet: func(context.Context, kubernetes.StatefulSet) error { return nil },
	})

	is.Equal(err, wantErr)
}

func TestWaitReadyUsesDeploymentWaiter(t *testing.T) {
	is := is.New(t)

	operations := []string{}
	managedApp := App{
		Namespace: "borq-system",
		Workload: Workload{
			Kind: DeploymentWorkload,
			Deployment: kubernetes.Deployment{
				Name: "whoami",
			},
		},
	}

	err := managedApp.waitReady(context.Background(), waitFuncs{
		deployment: func(_ context.Context, namespace string, name string) error {
			operations = append(operations, namespace+"/"+name)
			return nil
		},
	})

	is.NoErr(err)
	is.Equal(operations, []string{"borq-system/whoami"})
}

func TestWaitReadyUsesStatefulSetWaiter(t *testing.T) {
	is := is.New(t)

	operations := []string{}
	managedApp := App{
		Namespace: "borq-system",
		Workload: Workload{
			Kind: StatefulSetWorkload,
			StatefulSet: kubernetes.StatefulSet{
				Name: "vault",
			},
		},
	}

	err := managedApp.waitReady(context.Background(), waitFuncs{
		statefulSet: func(_ context.Context, namespace string, name string) error {
			operations = append(operations, namespace+"/"+name)
			return nil
		},
	})

	is.NoErr(err)
	is.Equal(operations, []string{"borq-system/vault"})
}
