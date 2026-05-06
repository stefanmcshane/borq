package app

import (
	"context"

	"github.com/stefanmcshane/borq/foundations/kubernetes"
)

// App is borq business intent for a deployable application.
type App struct {
	Name            string
	Namespace       string
	Labels          map[string]string
	ConfigMaps      []kubernetes.ConfigMap
	ServiceAccounts []kubernetes.ServiceAccount
	Roles           []kubernetes.Role
	RoleBindings    []kubernetes.RoleBinding
	Services        []kubernetes.Service
	Workload        Workload
}

// Workload is the app workload intent.
type Workload struct {
	Kind        WorkloadKind
	Deployment  kubernetes.Deployment
	StatefulSet kubernetes.StatefulSet
}

// WorkloadKind identifies the workload controller.
type WorkloadKind string

const (
	// DeploymentWorkload uses a Deployment.
	DeploymentWorkload WorkloadKind = "deployment"
	// StatefulSetWorkload uses a StatefulSet.
	StatefulSetWorkload WorkloadKind = "statefulset"
)

// Apply applies the app resources in business-defined order.
func (app App) Apply(ctx context.Context, connection kubernetes.Connection) error {
	return app.apply(ctx, applyFuncs{
		configMap:      connection.ConfigMaps().Apply,
		serviceAccount: connection.ServiceAccounts().Apply,
		role:           connection.Roles().Apply,
		roleBinding:    connection.RoleBindings().Apply,
		service:        connection.Services().Apply,
		deployment:     connection.Deployments().Apply,
		statefulSet:    connection.StatefulSets().Apply,
	})
}

// WaitReady waits for the app workload to be ready.
func (app App) WaitReady(ctx context.Context, connection kubernetes.Connection) error {
	return app.waitReady(ctx, waitFuncs{
		deployment:  connection.Deployments().WaitForAvailable,
		statefulSet: connection.StatefulSets().WaitForReady,
	})
}

func (app App) apply(ctx context.Context, funcs applyFuncs) error {
	for _, configMap := range app.ConfigMaps {
		if err := funcs.configMap(ctx, configMap); err != nil {
			return err
		}
	}
	for _, serviceAccount := range app.ServiceAccounts {
		if err := funcs.serviceAccount(ctx, serviceAccount); err != nil {
			return err
		}
	}
	for _, role := range app.Roles {
		if err := funcs.role(ctx, role); err != nil {
			return err
		}
	}
	for _, roleBinding := range app.RoleBindings {
		if err := funcs.roleBinding(ctx, roleBinding); err != nil {
			return err
		}
	}
	for _, service := range app.Services {
		if err := funcs.service(ctx, service); err != nil {
			return err
		}
	}

	switch app.Workload.Kind {
	case DeploymentWorkload:
		return funcs.deployment(ctx, app.Workload.Deployment)
	case StatefulSetWorkload:
		return funcs.statefulSet(ctx, app.Workload.StatefulSet)
	default:
		return nil
	}
}

func (app App) waitReady(ctx context.Context, funcs waitFuncs) error {
	switch app.Workload.Kind {
	case DeploymentWorkload:
		return funcs.deployment(ctx, app.Namespace, app.Workload.Deployment.Name)
	case StatefulSetWorkload:
		return funcs.statefulSet(ctx, app.Namespace, app.Workload.StatefulSet.Name)
	default:
		return nil
	}
}

type applyFuncs struct {
	configMap      func(context.Context, kubernetes.ConfigMap) error
	serviceAccount func(context.Context, kubernetes.ServiceAccount) error
	role           func(context.Context, kubernetes.Role) error
	roleBinding    func(context.Context, kubernetes.RoleBinding) error
	service        func(context.Context, kubernetes.Service) error
	deployment     func(context.Context, kubernetes.Deployment) error
	statefulSet    func(context.Context, kubernetes.StatefulSet) error
}

type waitFuncs struct {
	deployment  func(context.Context, string, string) error
	statefulSet func(context.Context, string, string) error
}
