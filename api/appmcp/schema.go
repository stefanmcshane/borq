package appmcp

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
)

// The OpenAPI library owns schemas and parameters as shared pointers.
func operationInput(inherited openapi3.Parameters, operation *openapi3.Operation) (*openapi3.Schema, error) {
	input := openapi3.NewObjectSchema().WithoutAdditionalProperties()
	parameters := append(slices.Clone(inherited), operation.Parameters...)
	for _, reference := range parameters {
		parameter := reference.Value
		if parameter.In != "path" && parameter.In != "query" {
			return nil, fmt.Errorf("unsupported %s parameter %s", parameter.In, parameter.Name)
		}
		if parameter.Schema == nil || parameter.Schema.Value == nil ||
			parameter.Schema.Value.Type.Includes("object") || parameter.Schema.Value.Type.Includes("array") {
			return nil, fmt.Errorf("parameter %s requires a scalar schema", parameter.Name)
		}
		if input.Properties[parameter.In] == nil {
			input.WithProperty(parameter.In, openapi3.NewObjectSchema().WithoutAdditionalProperties())
		}
		group := input.Properties[parameter.In].Value
		group.WithPropertyRef(parameter.Name, parameter.Schema)
		if parameter.Required && !slices.Contains(group.Required, parameter.Name) {
			group.Required = append(group.Required, parameter.Name)
		}
		if parameter.Required && !slices.Contains(input.Required, parameter.In) {
			input.Required = append(input.Required, parameter.In)
		}
	}
	if operation.RequestBody == nil {
		return input, nil
	}
	body := operation.RequestBody.Value
	content := body.Content.Get("application/json")
	if content == nil || content.Schema == nil {
		return nil, fmt.Errorf("request body requires an application/json schema")
	}
	input.WithPropertyRef("body", content.Schema)
	if body.Required {
		input.Required = append(input.Required, "body")
	}
	return input, nil
}

// toolSchema makes component references local to each advertised JSON Schema,
// including only the definitions reachable from that tool's inputs.
func toolSchema(input *openapi3.Schema, components openapi3.Schemas) (map[string]any, error) {
	data, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		return nil, err
	}
	definitions := map[string]any{}
	if err := localizeReferences(schema, components, definitions); err != nil {
		return nil, err
	}
	if len(definitions) > 0 {
		schema["$defs"] = definitions
	}
	return schema, nil
}

func localizeReferences(value any, components openapi3.Schemas, definitions map[string]any) error {
	switch value := value.(type) {
	case []any:
		for _, child := range value {
			if err := localizeReferences(child, components, definitions); err != nil {
				return err
			}
		}
	case map[string]any:
		if reference, ok := value["$ref"].(string); ok {
			name, found := strings.CutPrefix(reference, "#/components/schemas/")
			component := components[name]
			if !found || component == nil || component.Value == nil {
				return fmt.Errorf("unsupported schema reference %q", reference)
			}
			value["$ref"] = "#/$defs/" + name
			if _, exists := definitions[name]; !exists {
				// Mark before descending so recursive component references terminate.
				definitions[name] = nil
				data, err := json.Marshal(component.Value)
				if err != nil {
					return err
				}
				var definition map[string]any
				if err := json.Unmarshal(data, &definition); err != nil {
					return err
				}
				if err := localizeReferences(definition, components, definitions); err != nil {
					return err
				}
				definitions[name] = definition
			}
		}
		for _, child := range value {
			if err := localizeReferences(child, components, definitions); err != nil {
				return err
			}
		}
	}
	return nil
}
