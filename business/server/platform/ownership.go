package platform

import (
	"context"
	"errors"
	"fmt"

	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeclient "k8s.io/client-go/kubernetes"
)

// ErrTargetOwnership identifies a physical cluster owned by another registration.
// Package scope lets API workflows recognize this conflict with errors.Is.
var ErrTargetOwnership = errors.New("target cluster ownership conflict")

// CheckTargetOwnership reads a cluster's registration claim before accepting it.
// A missing claim is allowed; reconciliation claims it atomically before writes.
func CheckTargetOwnership(ctx context.Context, input TargetInput) error {
	if err := validateTargetIdentity(input); err != nil {
		return err
	}
	input.Settings.Namespace = SystemNamespace
	settings, err := normalizeSettings(input.Settings)
	if err != nil {
		return err
	}
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: input.KubeconfigPath, Context: input.Context, InCluster: input.InCluster}, logger.Logger{})
	if err != nil {
		return err
	}
	if err := rejectLegacyTargetOwner(ctx, connection); err != nil {
		return err
	}
	owner, err := connection.ConfigMaps().Get(ctx, settings.Namespace, "borq-platform-owner")
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read target cluster ownership: %w", err)
	}
	return targetOwnershipConflict(owner.Data, input)
}

// claimTargetOwner uses Kubernetes' atomic create to keep shared controllers and
// vault credentials attached to one registration, even across Borq servers.
func claimTargetOwner(ctx context.Context, connection kubernetes.Connection, namespace string, input TargetInput) error {
	if err := validateTargetIdentity(input); err != nil {
		return err
	}
	if err := rejectLegacyTargetOwner(ctx, connection); err != nil {
		return err
	}
	configuration := connection.RestConfig()
	// The Kubernetes SDK requires a configuration pointer when constructing clients.
	client, err := kubeclient.NewForConfig(&configuration)
	if err != nil {
		return fmt.Errorf("create target ownership client: %w", err)
	}
	immutable := true
	owner := corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "borq-platform-owner", Namespace: namespace, Labels: managedLabels()},
		// Kubernetes represents optional immutable configuration as a pointer.
		Immutable: &immutable,
		Data: map[string]string{
			"clusterId": input.ID, "organization": input.Organization, "name": input.Name,
		},
	}
	// Create requires an object pointer; unlike apply, this cannot replace an owner.
	_, err = client.CoreV1().ConfigMaps(namespace).Create(ctx, &owner, metav1.CreateOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("claim target cluster ownership: %w", err)
	}
	existing, err := client.CoreV1().ConfigMaps(namespace).Get(ctx, owner.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read target cluster ownership: %w", err)
	}
	return targetOwnershipConflict(existing.Data, input)
}

// Namespace changes must not make an immutable registration claim disappear.
// A legacy claim needs an explicit migration, even for the same registration.
func rejectLegacyTargetOwner(ctx context.Context, connection kubernetes.Connection) error {
	owner, err := connection.ConfigMaps().Get(ctx, "borq", "borq-platform-owner")
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read legacy target cluster ownership: %w", err)
	}
	return fmt.Errorf("%w: legacy registration in borq belongs to organization %q as cluster %q; migration is required before linking this cluster", ErrTargetOwnership, owner.Data["organization"], owner.Data["name"])
}

func targetOwnershipConflict(owner map[string]string, input TargetInput) error {
	if owner["clusterId"] != input.ID || owner["organization"] != input.Organization || owner["name"] != input.Name {
		return fmt.Errorf("%w: already owned by organization %q as cluster %q; use that registration instead of registering this physical cluster again", ErrTargetOwnership, owner["organization"], owner["name"])
	}
	return nil
}

func validateTargetIdentity(input TargetInput) error {
	if input.ID == "" || input.Organization == "" || input.Name == "" {
		return errors.New("target ownership requires cluster identity, organization and name")
	}
	return nil
}
