package reconciler

import (
	"context"
	"errors"
	"testing"

	"github.com/matryer/is"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
)

// mockHandler implements ComponentHandler for testing without a real K8s cluster.
type mockHandler struct {
	name        string
	applyCount  int
	deleteCount int
	lastApplied Resource
	lastDeleted Resource
	applyErr    error
	deleteErr   error
	plan        Plan
}

func (m *mockHandler) Name() string {
	if m.name == "" {
		return "mock"
	}
	return m.name
}

func (m *mockHandler) DefaultConfig() any {
	return &struct{}{}
}

func (m *mockHandler) Plan(config any) Plan {
	return m.plan
}

func (m *mockHandler) ApplyResource(_ context.Context, _ kubernetes.Connection, res Resource) error {
	m.applyCount++
	m.lastApplied = res
	return m.applyErr
}

func (m *mockHandler) DeleteResource(_ context.Context, _ kubernetes.Connection, res Resource) error {
	m.deleteCount++
	m.lastDeleted = res
	return m.deleteErr
}

func TestNewReconciler(t *testing.T) {
	is := is.New(t)

	r := NewReconciler(kubernetes.Connection{}, logger.Logger{})
	is.Equal(r.conn, kubernetes.Connection{})
}

func TestReconcileDryRunCreatesMissingResource(t *testing.T) {
	is := is.New(t)

	handler := &mockHandler{
		plan: Plan{
			ApplyOrder: []string{TypeService},
			Desired: []Resource{
				{Type: TypeService, Name: "svc1", Desired: kubernetes.Service{Name: "svc1"}},
			},
			Current: func(ctx context.Context, conn kubernetes.Connection) ([]Resource, error) {
				return nil, nil
			},
		},
	}

	r := NewReconciler(kubernetes.Connection{}, logger.Logger{})
	result, err := r.Reconcile(context.Background(), handler.Plan(nil), handler, true)
	is.NoErr(err)
	is.True(len(result.Diffs) == 1)
	is.True(result.Diffs[0].Changed)

	// dryRun should not apply
	is.Equal(handler.applyCount, 0)
}

func TestReconcileDryRunSkipsUnchangedResource(t *testing.T) {
	is := is.New(t)

	svc := kubernetes.Service{Name: "svc1", Namespace: "default", Labels: map[string]string{"app": "test"}, Selector: map[string]string{"app": "test"}}
	handler := &mockHandler{
		plan: Plan{
			ApplyOrder: []string{TypeService},
			Desired: []Resource{
				{Type: TypeService, Name: "svc1", Namespace: "default", Desired: svc},
			},
			Current: func(ctx context.Context, conn kubernetes.Connection) ([]Resource, error) {
				return []Resource{
					{Type: TypeService, Name: "svc1", Namespace: "default", Actual: svc},
				}, nil
			},
		},
	}

	r := NewReconciler(kubernetes.Connection{}, logger.Logger{})
	result, err := r.Reconcile(context.Background(), handler.Plan(nil), handler, true)
	is.NoErr(err)
	is.Equal(len(result.Diffs), 0)
}

func TestReconcileAppliesMissingResource(t *testing.T) {
	is := is.New(t)

	handler := &mockHandler{
		plan: Plan{
			ApplyOrder: []string{TypeService},
			Desired: []Resource{
				{Type: TypeService, Name: "svc1", Desired: kubernetes.Service{Name: "svc1"}},
			},
			Current: func(ctx context.Context, conn kubernetes.Connection) ([]Resource, error) {
				return nil, nil
			},
		},
	}

	r := NewReconciler(kubernetes.Connection{}, logger.Logger{})
	result, err := r.Reconcile(context.Background(), handler.Plan(nil), handler, false)
	is.NoErr(err)
	is.True(len(result.Diffs) == 1)
	is.Equal(handler.applyCount, 1)
	is.True(result.Applied)
}

func TestReconcileAppliesChangedResource(t *testing.T) {
	is := is.New(t)

	svc := kubernetes.Service{
		Name: "whoami", Namespace: "default",
		Labels: map[string]string{"app": "whoami"}, Selector: map[string]string{"app": "whoami"},
		Type:  kubernetes.ClusterIPService,
		Ports: []kubernetes.ServicePort{{Name: "http", Port: 80, TargetPort: 8080}},
	}

	handler := &mockHandler{
		plan: Plan{
			ApplyOrder: []string{TypeService},
			Desired: []Resource{
				{Type: TypeService, Name: "whoami", Namespace: "default", Desired: svc},
			},
			Current: func(ctx context.Context, conn kubernetes.Connection) ([]Resource, error) {
				liveSvc := svc
				liveSvc.Ports = []kubernetes.ServicePort{{Name: "http", Port: 80, TargetPort: 3000}}
				return []Resource{
					{Type: TypeService, Name: "whoami", Namespace: "default", Actual: liveSvc},
				}, nil
			},
		},
	}

	r := NewReconciler(kubernetes.Connection{}, logger.Logger{})
	result, err := r.Reconcile(context.Background(), handler.Plan(nil), handler, false)
	is.NoErr(err)
	is.True(len(result.Diffs) == 1)
	is.True(result.Diffs[0].Changed)
	is.Equal(handler.applyCount, 1)
	is.True(result.Applied)
}

func TestReconcileSkipsUnchangedResource(t *testing.T) {
	is := is.New(t)

	svc := kubernetes.Service{
		Name: "whoami", Namespace: "default",
		Labels: map[string]string{"app": "whoami"}, Selector: map[string]string{"app": "whoami"},
		Type:  kubernetes.ClusterIPService,
		Ports: []kubernetes.ServicePort{{Name: "http", Port: 80, TargetPort: 8080}},
	}

	handler := &mockHandler{
		plan: Plan{
			ApplyOrder: []string{TypeService},
			Desired: []Resource{
				{Type: TypeService, Name: "whoami", Namespace: "default", Desired: svc},
			},
			Current: func(ctx context.Context, conn kubernetes.Connection) ([]Resource, error) {
				return []Resource{
					{Type: TypeService, Name: "whoami", Namespace: "default", Actual: svc},
				}, nil
			},
		},
	}

	r := NewReconciler(kubernetes.Connection{}, logger.Logger{})
	result, err := r.Reconcile(context.Background(), handler.Plan(nil), handler, false)
	is.NoErr(err)
	is.Equal(len(result.Diffs), 0)
	is.Equal(handler.applyCount, 0)
	is.True(!result.Applied)
}

func TestReconcileAppliesInApplyOrder(t *testing.T) {
	is := is.New(t)

	handler := &mockHandler{
		plan: Plan{
			ApplyOrder: []string{TypeConfigMap, TypeService, TypeDeployment},
			Desired: []Resource{
				{Type: TypeDeployment, Name: "app", Desired: "deploy"},
				{Type: TypeConfigMap, Name: "cfg", Desired: "cm"},
				{Type: TypeService, Name: "svc", Desired: "svc"},
			},
			Current: func(ctx context.Context, conn kubernetes.Connection) ([]Resource, error) {
				return nil, nil
			},
		},
	}

	r := NewReconciler(kubernetes.Connection{}, logger.Logger{})
	result, err := r.Reconcile(context.Background(), handler.Plan(nil), handler, false)
	is.NoErr(err)
	is.Equal(len(result.Diffs), 3)
	is.Equal(handler.applyCount, 3)
	is.Equal(handler.lastApplied.Name, "app") // last applied should be last in order
}

func TestReconcileDeletesExtraResources(t *testing.T) {
	is := is.New(t)

	handler := &mockHandler{
		plan: Plan{
			ApplyOrder: []string{TypeService},
			Desired: []Resource{
				{Type: TypeService, Name: "keep", Desired: kubernetes.Service{Name: "keep"}},
			},
			Current: func(ctx context.Context, conn kubernetes.Connection) ([]Resource, error) {
				return []Resource{
					{Type: TypeService, Name: "keep", Actual: kubernetes.Service{Name: "keep"}},
					{Type: TypeService, Name: "extra", Actual: kubernetes.Service{Name: "extra"}},
				}, nil
			},
		},
	}

	r := NewReconciler(kubernetes.Connection{}, logger.Logger{})
	result, err := r.Reconcile(context.Background(), handler.Plan(nil), handler, false)
	is.NoErr(err)
	is.Equal(len(result.Diffs), 1) // only the delete shows in diffs (keep is unchanged)
	is.Equal(handler.deleteCount, 1)
	is.Equal(handler.lastDeleted.Name, "extra")
}

func TestReconcileDryRunDoesNotDeleteExtra(t *testing.T) {
	is := is.New(t)

	handler := &mockHandler{
		plan: Plan{
			ApplyOrder: []string{TypeService},
			Desired: []Resource{
				{Type: TypeService, Name: "keep", Desired: kubernetes.Service{Name: "keep"}},
			},
			Current: func(ctx context.Context, conn kubernetes.Connection) ([]Resource, error) {
				return []Resource{
					{Type: TypeService, Name: "keep", Actual: kubernetes.Service{Name: "keep"}},
					{Type: TypeService, Name: "extra", Actual: kubernetes.Service{Name: "extra"}},
				}, nil
			},
		},
	}

	r := NewReconciler(kubernetes.Connection{}, logger.Logger{})
	result, err := r.Reconcile(context.Background(), handler.Plan(nil), handler, true)
	is.NoErr(err)
	is.Equal(len(result.Diffs), 1) // extra resource shows in diff
	is.Equal(handler.deleteCount, 0)
}

func TestReconcileApplyErrorPropagates(t *testing.T) {
	is := is.New(t)

	handler := &mockHandler{
		applyErr: errors.New("apply failed"),
		plan: Plan{
			ApplyOrder: []string{TypeService},
			Desired: []Resource{
				{Type: TypeService, Name: "svc1", Desired: kubernetes.Service{Name: "svc1"}},
			},
			Current: func(ctx context.Context, conn kubernetes.Connection) ([]Resource, error) {
				return nil, nil
			},
		},
	}

	r := NewReconciler(kubernetes.Connection{}, logger.Logger{})
	_, err := r.Reconcile(context.Background(), handler.Plan(nil), handler, false)
	is.True(err != nil)

	var reconErr *ReconcileError
	is.True(errors.As(err, &reconErr))
	is.Equal(reconErr.Op, "reconcile")
	is.Equal(reconErr.Name, "svc1")
	is.Equal(reconErr.Err.Error(), "apply failed")
}

func TestReconcileApplyErrorOnExistingResourcePropagates(t *testing.T) {
	is := is.New(t)

	svc := kubernetes.Service{
		Name: "whoami", Namespace: "default",
		Labels: map[string]string{"app": "whoami"}, Selector: map[string]string{"app": "whoami"},
		Type:  kubernetes.ClusterIPService,
		Ports: []kubernetes.ServicePort{{Name: "http", Port: 80, TargetPort: 8080}},
	}

	handler := &mockHandler{
		applyErr: errors.New("apply changed failed"),
		plan: Plan{
			ApplyOrder: []string{TypeService},
			Desired: []Resource{
				{Type: TypeService, Name: "whoami", Namespace: "default", Desired: svc},
			},
			Current: func(ctx context.Context, conn kubernetes.Connection) ([]Resource, error) {
				liveSvc := svc
				liveSvc.Ports = []kubernetes.ServicePort{{Name: "http", Port: 80, TargetPort: 3000}}
				return []Resource{
					{Type: TypeService, Name: "whoami", Namespace: "default", Actual: liveSvc},
				}, nil
			},
		},
	}

	r := NewReconciler(kubernetes.Connection{}, logger.Logger{})
	_, err := r.Reconcile(context.Background(), handler.Plan(nil), handler, false)
	is.True(err != nil)

	var reconErr *ReconcileError
	is.True(errors.As(err, &reconErr))
	is.Equal(reconErr.Op, "reconcile")
	is.Equal(reconErr.Name, "whoami")
	is.Equal(reconErr.Err.Error(), "apply changed failed")
}

func TestReconcileDeleteErrorPropagates(t *testing.T) {
	is := is.New(t)

	handler := &mockHandler{
		deleteErr: errors.New("delete failed"),
		plan: Plan{
			ApplyOrder: []string{TypeService},
			Current: func(ctx context.Context, conn kubernetes.Connection) ([]Resource, error) {
				return []Resource{
					{Type: TypeService, Name: "extra", Actual: kubernetes.Service{Name: "extra"}},
				}, nil
			},
		},
	}

	r := NewReconciler(kubernetes.Connection{}, logger.Logger{})
	_, err := r.Reconcile(context.Background(), handler.Plan(nil), handler, false)
	is.True(err != nil)

	var reconErr *ReconcileError
	is.True(errors.As(err, &reconErr))
	is.Equal(reconErr.Op, "destroy")
	is.Equal(reconErr.Name, "extra")
	is.Equal(reconErr.Err.Error(), "delete failed")
}

func TestReconcileSkipsActualNilResources(t *testing.T) {
	is := is.New(t)

	handler := &mockHandler{
		plan: Plan{
			ApplyOrder: []string{TypeService},
			Desired: []Resource{
				{Type: TypeService, Name: "keep", Desired: kubernetes.Service{Name: "keep"}},
			},
			Current: func(ctx context.Context, conn kubernetes.Connection) ([]Resource, error) {
				return []Resource{
					{Type: TypeService, Name: "keep", Actual: nil},
					{Type: TypeService, Name: "extra", Actual: nil},
				}, nil
			},
		},
	}

	r := NewReconciler(kubernetes.Connection{}, logger.Logger{})
	result, err := r.Reconcile(context.Background(), handler.Plan(nil), handler, false)
	is.NoErr(err)
	is.Equal(len(result.Diffs), 1) // only the add/apply for keep
}

func TestDestroyDeletesInReverseOrder(t *testing.T) {
	is := is.New(t)

	handler := &mockHandler{
		plan: Plan{
			ApplyOrder: []string{TypeConfigMap, TypeService, TypeDeployment},
			Desired: []Resource{
				{Type: TypeConfigMap, Name: "cfg"},
				{Type: TypeService, Name: "svc"},
				{Type: TypeDeployment, Name: "app"},
			},
		},
	}

	r := NewReconciler(kubernetes.Connection{}, logger.Logger{})
	err := r.Destroy(context.Background(), handler.Plan(nil), handler)
	is.NoErr(err)
	is.Equal(handler.deleteCount, 3)

	// last deleted should be the first in apply order (ConfigMap)
	is.Equal(handler.lastDeleted.Type, TypeConfigMap)
	is.Equal(handler.lastDeleted.Name, "cfg")
}

func TestDestroyErrorPropagates(t *testing.T) {
	is := is.New(t)

	handler := &mockHandler{
		deleteErr: errors.New("destroy failed"),
		plan: Plan{
			ApplyOrder: []string{TypeService},
			Desired: []Resource{
				{Type: TypeService, Name: "svc"},
			},
		},
	}

	r := NewReconciler(kubernetes.Connection{}, logger.Logger{})
	err := r.Destroy(context.Background(), handler.Plan(nil), handler)
	is.True(err != nil)
	is.Equal(err.Error(), "destroy failed")
}

func TestDestroyEmptyPlan(t *testing.T) {
	is := is.New(t)

	handler := &mockHandler{
		plan: Plan{
			ApplyOrder: []string{},
		},
	}

	r := NewReconciler(kubernetes.Connection{}, logger.Logger{})
	err := r.Destroy(context.Background(), handler.Plan(nil), handler)
	is.NoErr(err)
	is.Equal(handler.deleteCount, 0)
}

func TestReconcileWithApplyOrderSubset(t *testing.T) {
	is := is.New(t)

	handler := &mockHandler{
		plan: Plan{
			ApplyOrder: []string{TypeService},
			Desired: []Resource{
				{Type: TypeService, Name: "svc1", Desired: kubernetes.Service{Name: "svc1"}},
				{Type: TypeConfigMap, Name: "cm1", Desired: kubernetes.ConfigMap{Name: "cm1"}},
			},
			Current: func(ctx context.Context, conn kubernetes.Connection) ([]Resource, error) {
				return nil, nil
			},
		},
	}

	r := NewReconciler(kubernetes.Connection{}, logger.Logger{})
	result, err := r.Reconcile(context.Background(), handler.Plan(nil), handler, true)
	is.NoErr(err)
	// Only Service is in ApplyOrder, so only that should be in diffs
	is.Equal(len(result.Diffs), 1)
	is.Equal(result.Diffs[0].Type, "Service")
}

func TestReconcileNoOpWhenEmptyExpected(t *testing.T) {
	is := is.New(t)

	handler := &mockHandler{
		plan: Plan{
			ApplyOrder: []string{},
			Current: func(ctx context.Context, conn kubernetes.Connection) ([]Resource, error) {
				return nil, nil
			},
		},
	}

	r := NewReconciler(kubernetes.Connection{}, logger.Logger{})
	result, err := r.Reconcile(context.Background(), handler.Plan(nil), handler, false)
	is.NoErr(err)
	is.Equal(len(result.Diffs), 0)
	is.True(!result.Applied)
}

func TestCoreApplierApplyResourceServiceAccount(t *testing.T) {
	is := is.New(t)

	clientset := fake.NewSimpleClientset(
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "test-sa", Namespace: "default"}},
	)
	conn := kubernetes.NewConnectionForTest(clientset)
	var applier CoreApplier

	err := applier.ApplyResource(context.Background(), conn, Resource{
		Type:    TypeServiceAccount,
		Name:    "test-sa",
		Desired: kubernetes.ServiceAccount{Name: "test-sa", Namespace: "default"},
	})
	is.NoErr(err)

	sa, err := clientset.CoreV1().ServiceAccounts("default").Get(context.Background(), "test-sa", metav1.GetOptions{})
	is.NoErr(err)
	is.Equal(sa.Name, "test-sa")
}

func TestCoreApplierApplyResourceClusterRole(t *testing.T) {
	is := is.New(t)

	clientset := fake.NewSimpleClientset(
		&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "test-cr"}},
	)
	conn := kubernetes.NewConnectionForTest(clientset)
	var applier CoreApplier

	err := applier.ApplyResource(context.Background(), conn, Resource{
		Type:    TypeClusterRole,
		Name:    "test-cr",
		Desired: kubernetes.ClusterRole{Name: "test-cr"},
	})
	is.NoErr(err)

	cr, err := clientset.RbacV1().ClusterRoles().Get(context.Background(), "test-cr", metav1.GetOptions{})
	is.NoErr(err)
	is.Equal(cr.Name, "test-cr")
}

func TestCoreApplierApplyResourceClusterRoleBinding(t *testing.T) {
	is := is.New(t)

	clientset := fake.NewSimpleClientset(
		&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "test-crb"}},
	)
	conn := kubernetes.NewConnectionForTest(clientset)
	var applier CoreApplier

	err := applier.ApplyResource(context.Background(), conn, Resource{
		Type:    TypeClusterRoleBinding,
		Name:    "test-crb",
		Desired: kubernetes.ClusterRoleBinding{Name: "test-crb", RoleRef: kubernetes.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "test-cr"}},
	})
	is.NoErr(err)

	crb, err := clientset.RbacV1().ClusterRoleBindings().Get(context.Background(), "test-crb", metav1.GetOptions{})
	is.NoErr(err)
	is.Equal(crb.Name, "test-crb")
}

func TestCoreApplierApplyResourceConfigMap(t *testing.T) {
	is := is.New(t)

	clientset := fake.NewSimpleClientset(
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "test-cm", Namespace: "default"}},
	)
	conn := kubernetes.NewConnectionForTest(clientset)
	var applier CoreApplier

	err := applier.ApplyResource(context.Background(), conn, Resource{
		Type:    TypeConfigMap,
		Name:    "test-cm",
		Desired: kubernetes.ConfigMap{Name: "test-cm", Namespace: "default"},
	})
	is.NoErr(err)

	cm, err := clientset.CoreV1().ConfigMaps("default").Get(context.Background(), "test-cm", metav1.GetOptions{})
	is.NoErr(err)
	is.Equal(cm.Name, "test-cm")
}

func TestCoreApplierApplyResourceService(t *testing.T) {
	is := is.New(t)

	clientset := fake.NewSimpleClientset(
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "test-svc", Namespace: "default"}},
	)
	conn := kubernetes.NewConnectionForTest(clientset)
	var applier CoreApplier

	err := applier.ApplyResource(context.Background(), conn, Resource{
		Type:    TypeService,
		Name:    "test-svc",
		Desired: kubernetes.Service{Name: "test-svc", Namespace: "default"},
	})
	is.NoErr(err)

	svc, err := clientset.CoreV1().Services("default").Get(context.Background(), "test-svc", metav1.GetOptions{})
	is.NoErr(err)
	is.Equal(svc.Name, "test-svc")
}

func TestCoreApplierApplyResourcePVC(t *testing.T) {
	is := is.New(t)

	clientset := fake.NewSimpleClientset(
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "test-pvc", Namespace: "default"}},
	)
	conn := kubernetes.NewConnectionForTest(clientset)
	var applier CoreApplier

	err := applier.ApplyResource(context.Background(), conn, Resource{
		Type:    TypePersistentVolumeClaim,
		Name:    "test-pvc",
		Desired: kubernetes.PersistentVolumeClaim{Name: "test-pvc", Namespace: "default", RequestedStorage: "1Gi"},
	})
	is.NoErr(err)

	pvc, err := clientset.CoreV1().PersistentVolumeClaims("default").Get(context.Background(), "test-pvc", metav1.GetOptions{})
	is.NoErr(err)
	is.Equal(pvc.Name, "test-pvc")
}

func TestCoreApplierApplyResourceDeployment(t *testing.T) {
	is := is.New(t)

	clientset := fake.NewSimpleClientset(
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "test-deploy", Namespace: "default"}},
	)
	conn := kubernetes.NewConnectionForTest(clientset)
	var applier CoreApplier

	err := applier.ApplyResource(context.Background(), conn, Resource{
		Type:    TypeDeployment,
		Name:    "test-deploy",
		Desired: kubernetes.Deployment{Name: "test-deploy", Namespace: "default", Replicas: 1},
	})
	is.NoErr(err)

	deploy, err := clientset.AppsV1().Deployments("default").Get(context.Background(), "test-deploy", metav1.GetOptions{})
	is.NoErr(err)
	is.Equal(deploy.Name, "test-deploy")
}

func TestCoreApplierApplyResourceStatefulSet(t *testing.T) {
	is := is.New(t)

	clientset := fake.NewSimpleClientset(
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "test-sts", Namespace: "default"}},
	)
	conn := kubernetes.NewConnectionForTest(clientset)
	var applier CoreApplier

	err := applier.ApplyResource(context.Background(), conn, Resource{
		Type:    TypeStatefulSet,
		Name:    "test-sts",
		Desired: kubernetes.StatefulSet{Name: "test-sts", Namespace: "default", Replicas: 1},
	})
	is.NoErr(err)

	sts, err := clientset.AppsV1().StatefulSets("default").Get(context.Background(), "test-sts", metav1.GetOptions{})
	is.NoErr(err)
	is.Equal(sts.Name, "test-sts")
}

func TestCoreApplierApplyResourceUnknownType(t *testing.T) {
	is := is.New(t)

	clientset := fake.NewSimpleClientset()
	conn := kubernetes.NewConnectionForTest(clientset)
	var applier CoreApplier

	err := applier.ApplyResource(context.Background(), conn, Resource{
		Type: "UnknownType",
		Name: "test",
	})
	is.True(err != nil)
}

func TestCoreApplierDeleteResourceServiceAccount(t *testing.T) {
	is := is.New(t)

	clientset := fake.NewSimpleClientset(&corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: "test-sa", Namespace: "default"},
	})
	conn := kubernetes.NewConnectionForTest(clientset)
	var applier CoreApplier

	err := applier.DeleteResource(context.Background(), conn, Resource{
		Type: TypeServiceAccount, Name: "test-sa", Namespace: "default",
	})
	is.NoErr(err)

	_, err = clientset.CoreV1().ServiceAccounts("default").Get(context.Background(), "test-sa", metav1.GetOptions{})
	is.True(err != nil)
}

func TestCoreApplierDeleteResourceClusterRole(t *testing.T) {
	is := is.New(t)

	clientset := fake.NewSimpleClientset(&rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cr"},
	})
	conn := kubernetes.NewConnectionForTest(clientset)
	var applier CoreApplier

	err := applier.DeleteResource(context.Background(), conn, Resource{
		Type: TypeClusterRole, Name: "test-cr",
	})
	is.NoErr(err)

	_, err = clientset.RbacV1().ClusterRoles().Get(context.Background(), "test-cr", metav1.GetOptions{})
	is.True(err != nil)
}

func TestCoreApplierDeleteResourceClusterRoleBinding(t *testing.T) {
	is := is.New(t)

	clientset := fake.NewSimpleClientset(&rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "test-crb"},
	})
	conn := kubernetes.NewConnectionForTest(clientset)
	var applier CoreApplier

	err := applier.DeleteResource(context.Background(), conn, Resource{
		Type: TypeClusterRoleBinding, Name: "test-crb",
	})
	is.NoErr(err)

	_, err = clientset.RbacV1().ClusterRoleBindings().Get(context.Background(), "test-crb", metav1.GetOptions{})
	is.True(err != nil)
}

func TestCoreApplierDeleteResourceConfigMap(t *testing.T) {
	is := is.New(t)

	clientset := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cm", Namespace: "default"},
	})
	conn := kubernetes.NewConnectionForTest(clientset)
	var applier CoreApplier

	err := applier.DeleteResource(context.Background(), conn, Resource{
		Type: TypeConfigMap, Name: "test-cm", Namespace: "default",
	})
	is.NoErr(err)

	_, err = clientset.CoreV1().ConfigMaps("default").Get(context.Background(), "test-cm", metav1.GetOptions{})
	is.True(err != nil)
}

func TestCoreApplierDeleteResourceService(t *testing.T) {
	is := is.New(t)

	clientset := fake.NewSimpleClientset(&corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "test-svc", Namespace: "default"},
	})
	conn := kubernetes.NewConnectionForTest(clientset)
	var applier CoreApplier

	err := applier.DeleteResource(context.Background(), conn, Resource{
		Type: TypeService, Name: "test-svc", Namespace: "default",
	})
	is.NoErr(err)

	_, err = clientset.CoreV1().Services("default").Get(context.Background(), "test-svc", metav1.GetOptions{})
	is.True(err != nil)
}

func TestCoreApplierDeleteResourcePVC(t *testing.T) {
	is := is.New(t)

	clientset := fake.NewSimpleClientset(&corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pvc", Namespace: "default"},
	})
	conn := kubernetes.NewConnectionForTest(clientset)
	var applier CoreApplier

	err := applier.DeleteResource(context.Background(), conn, Resource{
		Type: TypePersistentVolumeClaim, Name: "test-pvc", Namespace: "default",
	})
	is.NoErr(err)

	_, err = clientset.CoreV1().PersistentVolumeClaims("default").Get(context.Background(), "test-pvc", metav1.GetOptions{})
	is.True(err != nil)
}

func TestCoreApplierDeleteResourceDeployment(t *testing.T) {
	is := is.New(t)

	clientset := fake.NewSimpleClientset(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "test-deploy", Namespace: "default"},
	})
	conn := kubernetes.NewConnectionForTest(clientset)
	var applier CoreApplier

	err := applier.DeleteResource(context.Background(), conn, Resource{
		Type: TypeDeployment, Name: "test-deploy", Namespace: "default",
	})
	is.NoErr(err)

	_, err = clientset.AppsV1().Deployments("default").Get(context.Background(), "test-deploy", metav1.GetOptions{})
	is.True(err != nil)
}

func TestCoreApplierDeleteResourceStatefulSet(t *testing.T) {
	is := is.New(t)

	clientset := fake.NewSimpleClientset(&appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-sts", Namespace: "default"},
	})
	conn := kubernetes.NewConnectionForTest(clientset)
	var applier CoreApplier

	err := applier.DeleteResource(context.Background(), conn, Resource{
		Type: TypeStatefulSet, Name: "test-sts", Namespace: "default",
	})
	is.NoErr(err)

	_, err = clientset.AppsV1().StatefulSets("default").Get(context.Background(), "test-sts", metav1.GetOptions{})
	is.True(err != nil)
}

func TestCoreApplierDeleteResourceUnknownType(t *testing.T) {
	is := is.New(t)

	clientset := fake.NewSimpleClientset()
	conn := kubernetes.NewConnectionForTest(clientset)
	var applier CoreApplier

	err := applier.DeleteResource(context.Background(), conn, Resource{
		Type: "UnknownType",
		Name: "test",
	})
	is.True(err != nil)
}

func TestCoreApplierDeleteResourceNotFoundIsNoOp(t *testing.T) {
	is := is.New(t)

	clientset := fake.NewSimpleClientset()
	conn := kubernetes.NewConnectionForTest(clientset)
	var applier CoreApplier

	err := applier.DeleteResource(context.Background(), conn, Resource{
		Type: TypeConfigMap, Name: "does-not-exist", Namespace: "default",
	})
	is.NoErr(err)
}
