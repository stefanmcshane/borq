package kubernetes

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/stefanmcshane/borq/foundations/logger"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// ParseCRDs parses a multi-document YAML containing CustomResourceDefinitions.
func ParseCRDs(data []byte) ([]apiextensionsv1.CustomResourceDefinition, error) {
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	var crds []apiextensionsv1.CustomResourceDefinition
	for {
		var obj unstructured.Unstructured
		err := decoder.Decode(&obj)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decode CRD YAML: %w", err)
		}
		if obj.GetKind() != "CustomResourceDefinition" {
			continue
		}
		var crd apiextensionsv1.CustomResourceDefinition
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &crd); err != nil {
			return nil, fmt.Errorf("convert CRD %q: %w", obj.GetName(), err)
		}
		crds = append(crds, crd)
	}
	return crds, nil
}

// CRDManager manages CRD lifecycle: install, check usage, uninstall.
type CRDManager struct {
	objects ObjectManager
	log     logger.Logger
}

// NewCRDManager creates a CRDManager.
func NewCRDManager(objects ObjectManager, log logger.Logger) CRDManager {
	return CRDManager{objects: objects, log: log}
}

// EnsureInstalled installs the CRDs into the cluster, skipping any that already exist.
func (m CRDManager) EnsureInstalled(ctx context.Context, crds []apiextensionsv1.CustomResourceDefinition) error {
	for _, crd := range crds {
		obj := crd.DeepCopy()
		key := ctrlclient.ObjectKeyFromObject(obj)
		live := crd.DeepCopy()
		err := m.objects.client.Get(ctx, key, live)
		if err == nil {
			continue
		}
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("get CRD %q: %w", crd.Name, err)
		}
		if err := m.objects.client.Create(ctx, obj); err != nil {
			return fmt.Errorf("create CRD %q: %w", crd.Name, err)
		}
		m.log.Debug("crd_installed", logger.WithFields(
			logger.String("name", crd.Name),
		))
	}
	return nil
}

// HasInstances checks whether any custom resources of the given CRD type exist.
func (m CRDManager) HasInstances(ctx context.Context, crd apiextensionsv1.CustomResourceDefinition) (bool, error) {
	list := customResourceListForCRD(crd)
	if err := m.objects.client.List(ctx, list); err != nil {
		return false, fmt.Errorf("list %s/%s: %w", crd.Spec.Group, crd.Spec.Names.Kind, err)
	}
	return len(list.Items) > 0, nil
}

func customResourceListForCRD(crd apiextensionsv1.CustomResourceDefinition) *unstructured.UnstructuredList {
	version := crd.Spec.Versions[0].Name
	listKind := crd.Spec.Names.ListKind
	if listKind == "" {
		listKind = crd.Spec.Names.Kind + "List"
	}

	list := &unstructured.UnstructuredList{}
	list.SetAPIVersion(crd.Spec.Group + "/" + version)
	list.SetKind(listKind)
	return list
}

// UninstallIfUnused deletes CRDs that have no existing custom resources.
func (m CRDManager) UninstallIfUnused(ctx context.Context, crds []apiextensionsv1.CustomResourceDefinition) error {
	for _, crd := range crds {
		obj := crd.DeepCopy()
		key := ctrlclient.ObjectKeyFromObject(obj)
		live := crd.DeepCopy()
		err := m.objects.client.Get(ctx, key, live)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("get CRD %q: %w", crd.Name, err)
		}

		inUse, err := m.HasInstances(ctx, crd)
		if err != nil {
			return err
		}
		if inUse {
			m.log.Debug("crd_skip_uninstall_in_use", logger.WithFields(
				logger.String("name", crd.Name),
			))
			continue
		}

		if err := m.objects.client.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete CRD %q: %w", crd.Name, err)
		}
		m.log.Debug("crd_uninstalled", logger.WithFields(
			logger.String("name", crd.Name),
		))
	}
	return nil
}
