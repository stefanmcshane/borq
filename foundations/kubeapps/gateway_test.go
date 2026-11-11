package kubeapps

import (
	"testing"

	"github.com/matryer/is"
)

// TestGatewayControllerConditions verifies rollout policy waits for fresh
// controller acceptance and reference resolution, rather than resource creation.
// Real controller behavior belongs to TestK3sGatewayIntegration.
func TestGatewayControllerConditions(t *testing.T) {
	conditions := []any{
		map[string]any{"type": "Accepted", "status": "True", "observedGeneration": int64(2)},
		map[string]any{"type": "ResolvedRefs", "status": "True", "observedGeneration": int64(2)},
	}
	parent := map[string]any{"name": "frontend-public", "sectionName": "websecure"}
	route := map[string]any{
		"metadata": map[string]any{"generation": int64(2)},
		"spec":     map[string]any{"parentRefs": []any{parent}},
		"status":   map[string]any{"parents": []any{map[string]any{"parentRef": parent, "controllerName": "traefik.io/gateway-controller", "conditions": conditions}}},
	}
	gateway := map[string]any{
		"metadata": map[string]any{"generation": int64(2)},
		"spec":     map[string]any{"listeners": []any{map[string]any{"name": "websecure"}}},
		"status":   map[string]any{"conditions": conditions, "listeners": []any{map[string]any{"name": "websecure", "conditions": conditions}}},
	}
	for _, test := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"pending", func(value map[string]any) { delete(value, "status") }},
		{"stale", func(value map[string]any) { value["metadata"].(map[string]any)["generation"] = int64(3) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			live := copyMap(route)
			test.change(live)
			state, _ := httpRouteStatus(live, route)
			assert.Equal(state, "running")
			live = copyMap(gateway)
			test.change(live)
			state, _ = gatewayStatus(live, gateway)
			assert.Equal(state, "running")
		})
	}
	assert := is.New(t)
	state, _ := httpRouteStatus(route, route)
	assert.Equal(state, "succeeded")
	state, _ = gatewayStatus(gateway, gateway)
	assert.Equal(state, "succeeded")
	for _, change := range []func(map[string]any){
		func(parent map[string]any) { parent["controllerName"] = "foreign" },
		func(parent map[string]any) { parent["parentRef"] = map[string]any{"name": "foreign"} },
		func(parent map[string]any) { parent["conditions"].([]any)[1].(map[string]any)["status"] = "False" },
	} {
		live := copyMap(route)
		change(valueAt(live, "status", "parents").([]any)[0].(map[string]any))
		state, _ := httpRouteStatus(live, route)
		assert.Equal(state, "running")
	}
}
