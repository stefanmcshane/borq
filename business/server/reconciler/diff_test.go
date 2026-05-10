package reconciler

import (
	"encoding/json"
	"testing"

	"github.com/matryer/is"

	"github.com/stefanmcshane/borq/foundations/kubernetes"
)

func TestJoinPath(t *testing.T) {
	is := is.New(t)

	is.Equal(joinPath("", "spec"), "/spec")
	is.Equal(joinPath("/spec", "replicas"), "/spec/replicas")
}

func TestCollectKeysSkipsMetadataApiVersionKindStatus(t *testing.T) {
	is := is.New(t)

	live := map[string]any{"metadata": "x", "spec": "y"}
	desired := map[string]any{"apiVersion": "v1", "kind": "Pod", "status": "z"}
	keys := collectKeys(live, desired)

	is.Equal(len(keys), 1)
	is.Equal(keys[0], "spec")
}

func TestCollectKeysSortsAlphabetically(t *testing.T) {
	is := is.New(t)

	keys := collectKeys(
		map[string]any{"z": 1, "a": 2, "m": 3},
		map[string]any{},
	)
	is.Equal(len(keys), 3)
	is.Equal(keys[0], "a")
	is.Equal(keys[1], "m")
	is.Equal(keys[2], "z")
}

func TestValuesEqualString(t *testing.T) {
	is := is.New(t)

	is.True(valuesEqual("hello", "hello"))
	is.True(!valuesEqual("hello", "world"))
}

func TestValuesEqualFloat64(t *testing.T) {
	is := is.New(t)

	is.True(valuesEqual(float64(42), float64(42)))
	is.True(!valuesEqual(float64(42), float64(43)))
}

func TestValuesEqualBool(t *testing.T) {
	is := is.New(t)

	is.True(valuesEqual(true, true))
	is.True(!valuesEqual(true, false))
}

func TestValuesEqualFallbackToString(t *testing.T) {
	is := is.New(t)

	type custom struct{ V int }
	is.True(valuesEqual(custom{V: 1}, custom{V: 1}))
	is.True(!valuesEqual(custom{V: 1}, custom{V: 2}))
}

func TestComputePatchNoDiffEmpty(t *testing.T) {
	is := is.New(t)

	obj := map[string]any{"spec": map[string]any{"replicas": float64(3)}}
	patches := computePatch(obj, obj)
	is.Equal(len(patches), 0)
}

func TestComputePatchDetectsReplicasChange(t *testing.T) {
	is := is.New(t)

	live := map[string]any{"spec": map[string]any{"replicas": float64(3), "selector": map[string]any{"matchLabels": map[string]any{"app": "test"}}}}
	desired := map[string]any{"spec": map[string]any{"replicas": float64(5), "selector": map[string]any{"matchLabels": map[string]any{"app": "test"}}}}
	patches := computePatch(live, desired)
	is.Equal(len(patches), 1)
	is.Equal(patches[0].Op, "replace")
	is.Equal(patches[0].Path, "/spec/replicas")
	is.Equal(patches[0].Value, float64(5))
}

func TestComputePatchDetectsAddedField(t *testing.T) {
	is := is.New(t)

	live := map[string]any{"spec": map[string]any{"replicas": float64(3)}}
	desired := map[string]any{"spec": map[string]any{"replicas": float64(3), "selector": map[string]any{"matchLabels": map[string]any{"app": "test"}}}}
	patches := computePatch(live, desired)
	is.True(len(patches) > 0)
}

func TestComputePatchNilSliceEqualsEmptySlice(t *testing.T) {
	is := is.New(t)

	type Sub struct {
		Items []string `json:"items"`
	}
	type Container struct {
		Env []string `json:"env"`
	}
	type Spec struct {
		Containers []Container `json:"containers"`
	}
	type Resource struct {
		Spec Spec `json:"spec"`
	}

	live := Resource{Spec: Spec{Containers: []Container{{Env: nil}}}}
	desired := Resource{Spec: Spec{Containers: []Container{{Env: []string{}}}}}

	patches := computePatch(live, desired)
	is.Equal(len(patches), 0)

	patches = computePatch(desired, live)
	is.Equal(len(patches), 0)
}

func TestComputePatchEmptyDesiredStringSkipsDefaulted(t *testing.T) {
	is := is.New(t)

	type PVC struct {
		StorageClassName string `json:"storageClassName,omitempty"`
	}

	type Spec struct {
		PVC PVC `json:"pvcs"`
	}
	type Resource struct {
		Spec Spec `json:"spec"`
	}

	live := Resource{Spec: Spec{PVC: PVC{StorageClassName: "retain"}}}
	desired := Resource{Spec: Spec{PVC: PVC{StorageClassName: ""}}}

	patches := computePatch(live, desired)
	is.Equal(len(patches), 0)
}

func TestComputePatchDesiredAbsentFieldSkipsDefaulted(t *testing.T) {
	is := is.New(t)

	live := map[string]any{"storageClassName": "retain"}
	desired := map[string]any{}

	patches := computePatch(live, desired)
	is.Equal(len(patches), 0)
}

func TestComputePatchWithMarshalUnmarshalRoundTrip(t *testing.T) {
	is := is.New(t)

	live := kubernetes.StatefulSet{
		Name:      "vault",
		Namespace: "borq-system",
		Labels:    map[string]string{"app": "vault"},
		Selector:  map[string]string{"app": "vault"},
		Replicas:  1,
		Pods: kubernetes.PodTemplate{
			Labels: map[string]string{"app": "vault"},
			Containers: []kubernetes.Container{
				{
					Name:  "vault",
					Image: "hashicorp/vault:1.21.2",
					Env:   nil,
					Ports: []kubernetes.ContainerPort{{Name: "http", Port: 8200}},
				},
			},
		},
	}
	desired := live
	desired.Pods.Containers[0].Env = []kubernetes.EnvVar{}

	patches := computePatch(live, desired)
	is.Equal(len(patches), 0)
}

func TestComputePatchStringChangeDetected(t *testing.T) {
	is := is.New(t)

	type Spec struct {
		Image string `json:"image"`
	}
	type Resource struct {
		Spec Spec `json:"spec"`
	}

	live := Resource{Spec: Spec{Image: "nginx:1.0"}}
	desired := Resource{Spec: Spec{Image: "nginx:2.0"}}

	patches := computePatch(live, desired)
	is.Equal(len(patches), 1)
	is.Equal(patches[0].Op, "replace")
	is.Equal(patches[0].Value, "nginx:2.0")
}

func TestComputePatchWithStructHavingNilVsEmptyNested(t *testing.T) {
	is := is.New(t)

	type EnvVar struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	type Container struct {
		Env []EnvVar `json:"env,omitempty"`
	}
	type Spec struct {
		Containers []Container `json:"containers"`
	}
	type Resource struct {
		Spec Spec `json:"spec"`
	}

	live := Resource{Spec: Spec{Containers: []Container{{Env: nil}}}}
	desired := Resource{Spec: Spec{Containers: []Container{{Env: []EnvVar{}}}}}

	patches := computePatch(live, desired)
	is.Equal(len(patches), 0)
}

func TestDiffMapsKeyInLiveNotDesired(t *testing.T) {
	is := is.New(t)

	var patches []DiffPatch
	diffMaps("",
		map[string]any{"storageClassName": "retain"},
		map[string]any{},
		&patches, false)

	is.Equal(len(patches), 0)
}

func TestDiffMapsKeyInDesiredNotLive(t *testing.T) {
	is := is.New(t)

	var patches []DiffPatch
	diffMaps("",
		map[string]any{},
		map[string]any{"replicas": float64(3)},
		&patches, false)

	is.True(len(patches) > 0)
}

func TestDiffMapsBothHaveKeyWithDiff(t *testing.T) {
	is := is.New(t)

	var patches []DiffPatch
	diffMaps("",
		map[string]any{"replicas": float64(1)},
		map[string]any{"replicas": float64(3)},
		&patches, false)

	is.Equal(len(patches), 1)
	is.Equal(patches[0].Op, "replace")
}

func TestDiffMapsNestedMapsDiff(t *testing.T) {
	is := is.New(t)

	var patches []DiffPatch
	diffMaps("",
		map[string]any{"spec": map[string]any{"replicas": float64(1), "selector": map[string]any{"app": "vault"}}},
		map[string]any{"spec": map[string]any{"replicas": float64(3), "selector": map[string]any{"app": "vault"}}},
		&patches,
		false,
	)

	is.Equal(len(patches), 1)
	is.Equal(patches[0].Path, "/spec/replicas")
}

func TestDiffMapsExtraLiveKeySkipped(t *testing.T) {
	is := is.New(t)

	var patches []DiffPatch
	diffMaps("",
		map[string]any{"replicas": float64(3), "extraMap": map[string]any{"nested": "value"}},
		map[string]any{"replicas": float64(3)},
		&patches, false)

	is.Equal(len(patches), 0)
}

func TestDiffValuesBothNil(t *testing.T) {
	is := is.New(t)

	var patches []DiffPatch
	diffValues("/spec/replicas", nil, nil, &patches, false)
	is.Equal(len(patches), 0)
}

func TestDiffValuesBothStringsSame(t *testing.T) {
	is := is.New(t)

	var patches []DiffPatch
	diffValues("/spec/image", "nginx", "nginx", &patches, false)
	is.Equal(len(patches), 0)
}

func TestDiffValuesBothStringsDiff(t *testing.T) {
	is := is.New(t)

	var patches []DiffPatch
	diffValues("/spec/image", "nginx:1.0", "nginx:2.0", &patches, false)
	is.Equal(len(patches), 1)
	is.Equal(patches[0].Op, "replace")
}

func TestDiffValuesBothFloat64Same(t *testing.T) {
	is := is.New(t)

	var patches []DiffPatch
	diffValues("/spec/replicas", float64(3), float64(3), &patches, false)
	is.Equal(len(patches), 0)
}

func TestDiffValuesBothFloat64Diff(t *testing.T) {
	is := is.New(t)

	var patches []DiffPatch
	diffValues("/spec/replicas", float64(1), float64(3), &patches, false)
	is.Equal(len(patches), 1)
}

func TestDiffValuesBothBoolSame(t *testing.T) {
	is := is.New(t)

	var patches []DiffPatch
	diffValues("/spec/enabled", true, true, &patches, false)
	is.Equal(len(patches), 0)
}

func TestDiffValuesDesiredAbsentLiveNonEmptyString(t *testing.T) {
	is := is.New(t)

	var patches []DiffPatch
	diffValues("/spec/storageClassName", "retain", nil, &patches, false)
	is.Equal(len(patches), 0)
}

func TestDiffValuesDesiredAbsentLiveEmptySlice(t *testing.T) {
	is := is.New(t)

	var patches []DiffPatch
	diffValues("/spec/env", []any{}, nil, &patches, false)
	is.Equal(len(patches), 0)
}

func TestDiffValuesLiveAbsentDesiredEmptySlice(t *testing.T) {
	is := is.New(t)

	var patches []DiffPatch
	diffValues("/spec/env", nil, []any{}, &patches, false)
	is.Equal(len(patches), 0)
}

func TestDiffValuesEmptyDesiredStringNonEmptyLive(t *testing.T) {
	is := is.New(t)

	var patches []DiffPatch
	diffValues("/spec/storageClassName", "retain", "", &patches, false)
	is.Equal(len(patches), 0)
}

func TestDiffValuesDesiredAbsentLiveNonEmptyList(t *testing.T) {
	is := is.New(t)

	var patches []DiffPatch
	diffValues("/spec/items", []any{"a"}, nil, &patches, false)
	is.True(len(patches) > 0)
}

func TestDiffValuesDesiredAbsentLiveEmptyString(t *testing.T) {
	is := is.New(t)

	var patches []DiffPatch
	diffValues("/spec/name", "", nil, &patches, false)
	is.True(len(patches) > 0)
}

func TestDiffListsBothEmpty(t *testing.T) {
	is := is.New(t)

	var patches []DiffPatch
	diffLists("/spec/containers", []any{}, []any{}, &patches, false)
	is.Equal(len(patches), 0)
}

func TestDiffListsAddItem(t *testing.T) {
	is := is.New(t)

	var patches []DiffPatch
	diffLists("/spec/containers",
		[]any{},
		[]any{map[string]any{"name": "nginx"}},
		&patches, false)

	is.Equal(len(patches), 1)
	is.Equal(patches[0].Op, "add")
}

func TestDiffListsRemoveItem(t *testing.T) {
	is := is.New(t)

	var patches []DiffPatch
	diffLists("/spec/containers",
		[]any{map[string]any{"name": "nginx"}, map[string]any{"name": "sidecar"}},
		[]any{map[string]any{"name": "nginx"}},
		&patches, false)

	is.True(len(patches) >= 1)
	removeFound := false
	for _, p := range patches {
		if p.Op == "remove" {
			removeFound = true
			break
		}
	}
	is.True(removeFound)
}

func TestDiffListsReplaceItem(t *testing.T) {
	is := is.New(t)

	var patches []DiffPatch
	diffLists("/spec/containers",
		[]any{map[string]any{"image": "nginx:1.0"}},
		[]any{map[string]any{"image": "nginx:2.0"}},
		&patches, false)

	is.True(len(patches) >= 1)
}

func TestAddPatchForSimpleValue(t *testing.T) {
	is := is.New(t)

	type Obj struct {
		Name string `json:"name"`
	}
	patches := addPatchFor(Obj{Name: "test"})
	is.True(len(patches) == 1)
	is.Equal(patches[0].Op, "add")
	is.Equal(patches[0].Path, "/name")
	is.Equal(patches[0].Value, "test")
}

func TestAddPatchForNestedValue(t *testing.T) {
	is := is.New(t)

	type Spec struct {
		Replicas int32 `json:"replicas"`
	}
	type Obj struct {
		Spec Spec `json:"spec"`
	}
	patches := addPatchFor(Obj{Spec: Spec{Replicas: 3}})
	is.True(len(patches) >= 1)
	addCount := 0
	for _, p := range patches {
		if p.Op == "add" {
			addCount++
		}
	}
	is.Equal(addCount, len(patches))
}

func TestFlattenNestedMaps(t *testing.T) {
	is := is.New(t)

	var patches []DiffPatch
	flatten("", map[string]any{
		"spec": map[string]any{
			"replicas": float64(3),
		},
		"name": "test",
	}, &patches)

	is.Equal(len(patches), 2)

	paths := make(map[string]any)
	for _, p := range patches {
		paths[p.Path] = p.Value
	}
	is.Equal(paths["/name"], "test")
	is.Equal(paths["/spec/replicas"], float64(3))
}

func TestBuildResourceLookupNil(t *testing.T) {
	is := is.New(t)

	lookup := buildResourceLookup(nil)
	is.Equal(len(lookup), 0)
}

func TestBuildResourceLookupPopulatesMap(t *testing.T) {
	is := is.New(t)

	resources := []Resource{
		{Type: TypeService, Name: "svc1"},
		{Type: TypeConfigMap, Name: "cm1"},
	}
	lookup := buildResourceLookup(resources)
	is.Equal(len(lookup), 2)
	is.Equal(lookup["Service/svc1"].Type, TypeService)
	is.Equal(lookup["ConfigMap/cm1"].Type, TypeConfigMap)
}

func TestResourceKey(t *testing.T) {
	is := is.New(t)

	is.Equal(resourceKey(TypeService, "my-svc"), "Service/my-svc")
	is.Equal(resourceKey(TypeConfigMap, "my-cm"), "ConfigMap/my-cm")
}

func TestResourceInExpectedFound(t *testing.T) {
	is := is.New(t)

	desired := []Resource{
		{Type: TypeService, Name: "svc1"},
	}

	res, found := resourceInExpected("Service/svc1", desired)
	is.True(found)
	is.Equal(res.Type, TypeService)
	is.Equal(res.Name, "svc1")
}

func TestResourceInExpectedNotFound(t *testing.T) {
	is := is.New(t)

	desired := []Resource{
		{Type: TypeService, Name: "svc1"},
	}

	_, found := resourceInExpected("ConfigMap/cm1", desired)
	is.True(!found)
}

func TestResourceInExpectedNilExpected(t *testing.T) {
	is := is.New(t)

	_, found := resourceInExpected("Service/svc1", nil)
	is.True(!found)
}

func TestComputePatchWithStructAndJsonTags(t *testing.T) {
	is := is.New(t)

	type Port struct {
		Port int32  `json:"port"`
		Name string `json:"name,omitempty"`
	}
	type Container struct {
		Name  string `json:"name"`
		Image string `json:"image"`
		Ports []Port `json:"ports,omitempty"`
	}
	type Resource struct {
		APIVersion string         `json:"apiVersion"`
		Kind       string         `json:"kind"`
		Metadata   map[string]any `json:"metadata,omitempty"`
		Spec       struct {
			Containers []Container `json:"containers"`
		} `json:"spec"`
	}

	live := Resource{
		APIVersion: "apps/v1",
		Kind:       "StatefulSet",
		Metadata:   map[string]any{"name": "vault", "namespace": "borq-system"},
	}
	live.Spec.Containers = []Container{
		{Name: "vault", Image: "hashicorp/vault:1.21.2", Ports: []Port{{Name: "http", Port: 8200}}},
	}

	desired := live

	patches := computePatch(live, desired)
	is.Equal(len(patches), 0)
}

func TestComputePatchJsonMarshalErrorReturnsEmpty(t *testing.T) {
	is := is.New(t)

	patches := computePatch(make(chan int), make(chan int))
	is.Equal(len(patches), 0)
}

func TestComputePatchAddsNewKeyToNestedMap(t *testing.T) {
	is := is.New(t)

	live := map[string]any{"spec": map[string]any{"replicas": float64(1)}}
	desired := map[string]any{"spec": map[string]any{"replicas": float64(1), "selector": map[string]any{"app": "test"}}}

	patches := computePatch(live, desired)
	is.True(len(patches) > 0)
	hasAdd := false
	for _, p := range patches {
		if p.Op == "add" {
			hasAdd = true
			break
		}
	}
	is.True(hasAdd)
}

func TestComputePatchUnstructuredLiveWithExtraFields(t *testing.T) {
	is := is.New(t)

	type Container struct {
		Name  string `json:"name"`
		Image string `json:"image"`
		Ports []struct {
			ContainerPort int32  `json:"containerPort"`
			Protocol      string `json:"protocol"`
		} `json:"ports,omitempty"`
	}

	type Spec struct {
		Containers []Container `json:"containers"`
	}

	type Deployment struct {
		APIVersion string         `json:"apiVersion"`
		Kind       string         `json:"kind"`
		Metadata   map[string]any `json:"metadata,omitempty"`
		Spec       Spec           `json:"spec"`
	}

	desired := Deployment{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
		Spec: Spec{
			Containers: []Container{
				{Name: "test", Image: "nginx", Ports: nil},
			},
		},
	}

	live := map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]any{
			"name":            "test",
			"namespace":       "default",
			"resourceVersion": "12345",
			"generation":      float64(1),
			"uid":             "abc-123",
		},
		"spec": map[string]any{
			"containers": []any{
				map[string]any{
					"name":                     "test",
					"image":                    "nginx",
					"terminationMessagePath":   "/dev/termination-log",
					"terminationMessagePolicy": "File",
					"imagePullPolicy":          "IfNotPresent",
				},
			},
			"dnsPolicy":                     "ClusterFirst",
			"restartPolicy":                 "Always",
			"schedulerName":                 "default-scheduler",
			"terminationGracePeriodSeconds": float64(30),
		},
	}

	patches := computePatch(live, desired)
	is.Equal(len(patches), 0)
}

func TestCollectKeysLiveNilDesiredHasKeys(t *testing.T) {
	is := is.New(t)

	keys := collectKeys(nil, map[string]any{"spec": "x"})
	is.True(len(keys) > 0)
}

func TestCollectKeysLiveHasKeysDesiredNil(t *testing.T) {
	is := is.New(t)

	keys := collectKeys(map[string]any{"spec": "x"}, nil)
	is.True(len(keys) > 0)
}

func TestValuesEqualWithNil(t *testing.T) {
	is := is.New(t)

	is.True(!valuesEqual(nil, "something"))
	is.True(!valuesEqual("something", nil))
	is.True(valuesEqual(nil, nil))
}

func TestAddPatchComplexObject(t *testing.T) {
	is := is.New(t)

	type SubSpec struct {
		Field string `json:"field"`
	}
	type Spec struct {
		Sub SubSpec `json:"sub"`
	}
	type Obj struct {
		Spec Spec `json:"spec"`
	}

	patches := addPatchFor(Obj{Spec: Spec{Sub: SubSpec{Field: "value"}}})
	is.Equal(len(patches), 1)
	is.Equal(patches[0].Path, "/spec/sub/field")
	is.Equal(patches[0].Value, "value")
}

func TestComputePatchWithNonEmptyDesiredString(t *testing.T) {
	is := is.New(t)

	type PVC struct {
		StorageClassName *string `json:"storageClassName,omitempty"`
	}
	type Spec struct {
		PVC PVC `json:"pvc"`
	}
	type Resource struct {
		Spec Spec `json:"spec"`
	}

	sc := "fast"
	desired := Resource{Spec: Spec{PVC: PVC{StorageClassName: &sc}}}
	live := Resource{Spec: Spec{PVC: PVC{StorageClassName: &sc}}}

	patches := computePatch(live, desired)
	is.Equal(len(patches), 0)
}

func TestAddValueWithMap(t *testing.T) {
	is := is.New(t)

	var patches []DiffPatch
	addValue("/spec", map[string]any{"replicas": float64(3), "name": "test"}, &patches)

	is.Equal(len(patches), 2)
	for _, p := range patches {
		is.Equal(p.Op, "add")
	}
}

func TestAddValueWithScalar(t *testing.T) {
	is := is.New(t)

	var patches []DiffPatch
	addValue("/spec/replicas", float64(3), &patches)

	is.Equal(len(patches), 1)
	is.Equal(patches[0].Op, "add")
	is.Equal(patches[0].Path, "/spec/replicas")
	is.Equal(patches[0].Value, float64(3))
}

func TestFlattenEmptyMap(t *testing.T) {
	is := is.New(t)

	var patches []DiffPatch
	flatten("", map[string]any{}, &patches)
	is.Equal(len(patches), 0)
}

func TestComputePatchWithActualKubernetesTypes(t *testing.T) {
	is := is.New(t)

	svc := kubernetes.Service{
		Name:      "test",
		Namespace: "default",
		Labels:    map[string]string{"app": "test"},
		Selector:  map[string]string{"app": "test"},
		Type:      kubernetes.ClusterIPService,
		Ports: []kubernetes.ServicePort{
			{Name: "http", Port: 80, TargetPort: 8080},
		},
	}

	data, err := json.Marshal(svc)
	is.NoErr(err)

	var live, desired map[string]any
	is.NoErr(json.Unmarshal(data, &live))
	is.NoErr(json.Unmarshal(data, &desired))

	patches := computePatch(live, desired)
	is.Equal(len(patches), 0)
}

func TestDiffMapsSkipsMetadataApiVersionKind(t *testing.T) {
	is := is.New(t)

	var patches []DiffPatch
	diffMaps("",
		map[string]any{
			"metadata":   map[string]any{"name": "test"},
			"apiVersion": "v1",
			"kind":       "Pod",
			"spec":       map[string]any{"replicas": float64(1)},
		},
		map[string]any{
			"metadata":   map[string]any{"name": "test"},
			"apiVersion": "v1",
			"kind":       "Pod",
			"spec":       map[string]any{"replicas": float64(3)},
		},
		&patches,
		false,
	)

	is.Equal(len(patches), 1)
	is.Equal(patches[0].Path, "/spec/replicas")
}

func TestComputePatchEmptyStructFieldDesired(t *testing.T) {
	is := is.New(t)

	type EnvVar struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	type Container struct {
		Env []EnvVar `json:"env,omitempty"`
	}
	type Spec struct {
		Template struct {
			Containers []Container `json:"containers"`
		} `json:"template"`
	}
	type Resource struct {
		Spec Spec `json:"spec"`
	}

	desired := Resource{}
	desired.Spec.Template.Containers = []Container{{Env: []EnvVar{}}}

	sc := "retain"
	_ = sc
	live := Resource{}
	live.Spec.Template.Containers = []Container{{Env: nil}}

	patches := computePatch(live, desired)
	is.Equal(len(patches), 0)
}
