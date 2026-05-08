package reconciler

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
			if liveStr, ok := liveVal.(string); ok && liveStr != "" {
				continue
			}
			*patches = append(*patches, DiffPatch{Op: "remove", Path: path})
		} else {
			diffValues(path, liveVal, desiredVal, patches)
		}
	}
}

func diffValues(path string, live, desired any, patches *[]DiffPatch) {
	if desired == nil {
		if liveList, ok := live.([]any); ok && len(liveList) == 0 {
			return
		}
		if liveStr, ok := live.(string); ok && liveStr != "" {
			return
		}
	}
	if live == nil {
		if desiredList, ok := desired.([]any); ok && len(desiredList) == 0 {
			return
		}
	}

	desiredStr, desiredIsStr := desired.(string)
	liveStr, liveIsStr := live.(string)
	if desiredIsStr && desiredStr == "" && liveIsStr && liveStr != "" {
		return
	}

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

func buildResourceLookup(resources []Resource) map[string]Resource {
	lookup := make(map[string]Resource, len(resources))
	for _, res := range resources {
		lookup[resourceKey(res.Type, res.Name)] = res
	}
	return lookup
}

func resourceKey(resType, name string) string {
	return resType + "/" + name
}

func resourceInExpected(key string, desired []Resource) (Resource, bool) {
	for _, res := range desired {
		if resourceKey(res.Type, res.Name) == key {
			return res, true
		}
	}
	return Resource{}, false
}
