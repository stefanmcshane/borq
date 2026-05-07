package managementreconcile

import (
	"encoding/json"
	"fmt"
	"sort"
)

func computePatch(live, desired any) []DiffPatch {
	liveData, _ := json.Marshal(live)
	desiredData, _ := json.Marshal(desired)

	var liveMap, desiredMap map[string]any
	_ = json.Unmarshal(liveData, &liveMap)
	_ = json.Unmarshal(desiredData, &desiredMap)

	var patches []DiffPatch
	diffMaps("", liveMap, desiredMap, &patches)
	return patches
}

func addPatchFor(obj any) []DiffPatch {
	data, _ := json.Marshal(obj)
	var raw map[string]any
	_ = json.Unmarshal(data, &raw)
	var patches []DiffPatch
	flatten("", raw, &patches)
	for i := range patches {
		patches[i].Op = "add"
	}
	return patches
}

func diffMaps(prefix string, live, desired map[string]any, patches *[]DiffPatch) {
	keys := collectKeys(live, desired)
	for _, key := range keys {
		path := joinPath(prefix, key)
		liveVal, liveOK := live[key]
		desiredVal, desiredOK := desired[key]

		if !liveOK && desiredOK {
			addValue(path, desiredVal, patches)
		} else if liveOK && !desiredOK {
			*patches = append(*patches, DiffPatch{Op: "remove", Path: path})
		} else {
			diffValues(path, liveVal, desiredVal, patches)
		}
	}
}

func diffValues(path string, live, desired any, patches *[]DiffPatch) {
	liveMap, liveIsMap := live.(map[string]any)
	desiredMap, desiredIsMap := desired.(map[string]any)
	if liveIsMap && desiredIsMap {
		diffMaps(path, liveMap, desiredMap, patches)
		return
	}

	liveList, liveIsList := live.([]any)
	desiredList, desiredIsList := desired.([]any)
	if liveIsList && desiredIsList {
		diffLists(path, liveList, desiredList, patches)
		return
	}

	if !valuesEqual(live, desired) {
		*patches = append(*patches, DiffPatch{Op: "replace", Path: path, Value: desired})
	}
}

func diffLists(prefix string, live, desired []any, patches *[]DiffPatch) {
	maxLen := len(live)
	if len(desired) > maxLen {
		maxLen = len(desired)
	}

	for i := 0; i < maxLen; i++ {
		path := fmt.Sprintf("%s/%d", prefix, i)
		if i >= len(live) {
			addValue(path, desired[i], patches)
		} else if i >= len(desired) {
			*patches = append(*patches, DiffPatch{Op: "remove", Path: path})
		} else {
			diffValues(path, live[i], desired[i], patches)
		}
	}
}

func addValue(path string, val any, patches *[]DiffPatch) {
	if valMap, ok := val.(map[string]any); ok {
		var subPatches []DiffPatch
		flatten(path, valMap, &subPatches)
		for i := range subPatches {
			subPatches[i].Op = "add"
		}
		*patches = append(*patches, subPatches...)
	} else {
		*patches = append(*patches, DiffPatch{Op: "add", Path: path, Value: val})
	}
}

func flatten(prefix string, m map[string]any, patches *[]DiffPatch) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, key := range keys {
		path := joinPath(prefix, key)
		val := m[key]
		if subMap, ok := val.(map[string]any); ok {
			flatten(path, subMap, patches)
		} else {
			*patches = append(*patches, DiffPatch{Op: "add", Path: path, Value: val})
		}
	}
}

func joinPath(prefix, key string) string {
	if prefix == "" {
		return "/" + key
	}
	return prefix + "/" + key
}

func collectKeys(a, b map[string]any) []string {
	seen := make(map[string]bool)
	for k := range a {
		seen[k] = true
	}
	for k := range b {
		seen[k] = true
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		if k == "metadata" || k == "apiVersion" || k == "kind" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func valuesEqual(a, b any) bool {
	aStr, aOK := a.(string)
	bStr, bOK := b.(string)
	if aOK && bOK {
		return aStr == bStr
	}

	aF, aOK := a.(float64)
	bF, bOK := b.(float64)
	if aOK && bOK {
		return aF == bF
	}

	aB, aOK := a.(bool)
	bB, bOK := b.(bool)
	if aOK && bOK {
		return aB == bB
	}

	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}

func buildResourceLookup(infra *Infrastructure) map[string]ResourceState {
	if infra == nil {
		return nil
	}
	lookup := make(map[string]ResourceState, len(infra.Resources))
	for _, res := range infra.Resources {
		lookup[resourceKey(res.Kind, res.Name)] = res
	}
	return lookup
}

func resourceKey(kind Kind, name string) string {
	return string(kind) + "/" + name
}

func resourceInExpected(key string, expected *Infrastructure) (ResourceState, bool) {
	for _, res := range expected.Resources {
		if resourceKey(res.Kind, res.Name) == key {
			return res, true
		}
	}
	return ResourceState{}, false
}
