package appmcp

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
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

// operationOutput describes the existing status/body envelope. The OpenAPI
// library owns operation and schema values as shared pointers.
func operationOutput(operation *openapi3.Operation) (*openapi3.Schema, error) {
	if operation.Responses == nil || operation.Responses.Len() == 0 {
		return nil, fmt.Errorf("operation requires response schemas")
	}
	output := openapi3.NewObjectSchema()
	for _, code := range slices.Sorted(maps.Keys(operation.Responses.Map())) {
		status, err := strconv.Atoi(code)
		if err != nil || status < 100 || status > 599 {
			return nil, fmt.Errorf("response %s requires a concrete HTTP status", code)
		}
		response := operation.Responses.Value(code).Value
		if response == nil {
			return nil, fmt.Errorf("response %s is unresolved", code)
		}
		content := response.Content.Get("application/json")
		if content == nil || content.Schema == nil || content.Schema.Value == nil {
			return nil, fmt.Errorf("response %s requires an application/json schema", code)
		}
		variant := openapi3.NewObjectSchema().WithoutAdditionalProperties().
			WithProperty("status", openapi3.NewIntegerSchema().WithEnum(status)).
			WithPropertyRef("body", content.Schema).
			WithRequired([]string{"status", "body"})
		if response.Description != nil {
			variant.Description = *response.Description
		}
		output.AnyOf = append(output.AnyOf, openapi3.NewSchemaRef("", variant))
	}
	// Readiness guards can return problems before the operation handler runs;
	// HTTP failures can also return plain text instead of a JSON problem.
	failure := openapi3.NewObjectSchema().WithoutAdditionalProperties().
		WithProperty("status", openapi3.NewIntegerSchema().WithMin(400).WithMax(599)).
		WithProperty("body", &openapi3.Schema{AnyOf: openapi3.SchemaRefs{
			openapi3.NewSchemaRef("#/components/schemas/Problem", nil),
			openapi3.NewSchemaRef("", openapi3.NewStringSchema()),
		}}).
		WithRequired([]string{"status", "body"})
	failure.Description = "API or management readiness failure."
	output.AnyOf = append(output.AnyOf, openapi3.NewSchemaRef("", failure))
	return output, nil
}

// toolSchema makes component references local to each advertised JSON Schema,
// including only the definitions reachable from that tool's input or output.
func toolSchema(source *openapi3.Schema, components openapi3.Schemas) (map[string]any, error) {
	data, err := json.Marshal(source)
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
