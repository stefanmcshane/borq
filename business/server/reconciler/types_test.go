package reconciler

import (
	"errors"
	"testing"

	"github.com/matryer/is"
)

func TestReconcileErrorMessage(t *testing.T) {
	is := is.New(t)

	err := &ReconcileError{
		Type: TypeService, Namespace: "default", Name: "my-svc",
		Op: "reconcile", Err: errors.New("permission denied"),
	}
	is.Equal(err.Error(), "reconcile Service/my-svc: permission denied")
}

func TestReconcileErrorUnwrap(t *testing.T) {
	is := is.New(t)

	inner := errors.New("inner error")
	err := &ReconcileError{Err: inner}
	is.Equal(errors.Unwrap(err), inner)
}

func TestReconcileErrorUnwrapNil(t *testing.T) {
	is := is.New(t)

	err := &ReconcileError{}
	is.Equal(errors.Unwrap(err), nil)
}

func TestReconcileErrorFields(t *testing.T) {
	is := is.New(t)

	err := &ReconcileError{
		Type: TypeStatefulSet, Namespace: "borq-system", Name: "openbao",
		Op: "destroy", Err: errors.New("timeout"),
	}
	fields := err.Fields()
	is.Equal(len(fields), 4)

	is.Equal(fields[0].Key, "resource_type")
	is.Equal(fields[0].Value, "StatefulSet")
	is.Equal(fields[1].Key, "resource_namespace")
	is.Equal(fields[1].Value, "borq-system")
	is.Equal(fields[2].Key, "resource_name")
	is.Equal(fields[2].Value, "openbao")
	is.Equal(fields[3].Key, "op")
	is.Equal(fields[3].Value, "destroy")
}

func TestReconcileErrorAs(t *testing.T) {
	is := is.New(t)

	inner := errors.New("inner")
	wrapped := &ReconcileError{Err: inner}

	var target *ReconcileError
	is.True(errors.As(wrapped, &target))
	is.Equal(target.Type, "")
}

func TestReconcileResult(t *testing.T) {
	is := is.New(t)

	result := ReconcileResult{
		Diffs:   []ResourceDiff{{Type: "Service", Name: "svc", Changed: true}},
		Applied: true,
	}
	is.Equal(len(result.Diffs), 1)
	is.True(result.Applied)
}
