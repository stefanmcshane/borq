package cli

import (
	"fmt"
	"math"
	"regexp/syntax"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/getkin/kin-openapi/openapi3"
	"gopkg.in/yaml.v3"
)

// configurationTemplate keeps the API schema as the source of truth for editable
// fields. Schema pointers preserve reference identity while detecting recursion.
func configurationTemplate(schema *openapi3.Schema) (*yaml.Node, error) {
	node, err := templateNode([]*openapi3.Schema{schema}, make(map[*openapi3.Schema]bool))
	if err == nil && schema.Description != "" {
		node.HeadComment = strings.TrimSpace(schema.Description + "\n" + node.HeadComment)
	}
	return node, err
}

func templateNode(schemas []*openapi3.Schema, ancestors map[*openapi3.Schema]bool) (*yaml.Node, error) {
	for _, schema := range schemas {
		if schema == nil {
			return nil, fmt.Errorf("unresolved template schema")
		}
		if templateCompositionCycle(schema, make(map[*openapi3.Schema]bool)) {
			return nil, fmt.Errorf("recursive schema composition has no bounded placeholder")
		}
		if ancestors[schema] {
			for _, value := range []any{nil, map[string]any{}, []any{}} {
				node, err := templateValue(value)
				if err == nil && templateMatches(node, schemas) {
					node.HeadComment = "Recursive structure; expand this value as needed."
					return node, nil
				}
			}
			return nil, fmt.Errorf("recursive schema has no finite empty placeholder")
		}
	}
	for _, schema := range schemas {
		ancestors[schema] = true
	}
	defer func() {
		for _, schema := range schemas {
			delete(ancestors, schema)
		}
	}()

	for index, schema := range schemas {
		if len(schema.AllOf) == 0 {
			continue
		}
		base := *schema
		base.AllOf = nil
		expanded := append([]*openapi3.Schema{}, schemas[:index]...)
		expanded = append(expanded, &base)
		expanded = append(expanded, schemas[index+1:]...)
		for _, reference := range schema.AllOf {
			expanded = append(expanded, reference.Value)
		}
		compositionAncestors := make(map[*openapi3.Schema]bool, len(ancestors))
		for ancestor := range ancestors {
			compositionAncestors[ancestor] = true
		}
		for _, current := range schemas {
			delete(compositionAncestors, current)
		}
		compositionAncestors[schema] = true
		node, err := templateNode(expanded, compositionAncestors)
		if err != nil {
			return nil, err
		}
		if !templateMatches(node, schemas) {
			return nil, fmt.Errorf("allOf constraints cannot produce a valid placeholder")
		}
		return node, nil
	}

	kind := ""
	for _, schema := range schemas {
		for _, candidate := range schema.Type.Slice() {
			if candidate != "null" {
				kind = candidate
				break
			}
		}
		if kind != "" {
			break
		}
		if len(schema.Properties) > 0 || schema.AdditionalProperties.Schema != nil {
			kind = "object"
			break
		}
		if schema.Items != nil {
			kind = "array"
			break
		}
	}
	for _, schema := range schemas {
		if kind != "object" && kind != "array" {
			continue
		}
		for _, value := range schema.Enum {
			node, err := templateValue(value)
			if err == nil && templateMatches(node, schemas) {
				return node, nil
			}
		}
	}
	if kind == "object" {
		return templateObject(schemas, ancestors)
	}
	if kind == "array" {
		return templateArray(schemas, ancestors)
	}

	var values []any
	for _, schema := range schemas {
		if schema.Default != nil {
			values = append(values, schema.Default)
		}
		values = append(values, schema.Enum...)
	}
	for _, value := range values {
		node, err := templateValue(value)
		if err == nil && templateMatches(node, schemas) {
			return node, nil
		}
	}
	switch kind {
	case "string":
		strings, err := templateStrings(schemas)
		if err != nil {
			return nil, err
		}
		for _, value := range strings {
			values = append(values, value)
		}
	case "integer", "number":
		values = append(values, templateNumbers(schemas, kind == "integer")...)
	case "boolean":
		values = append(values, false, true)
	default:
		values = append(values, map[string]any{}, "replace-me", nil)
	}
	for _, value := range values {
		node, err := templateValue(value)
		if err == nil && templateMatches(node, schemas) {
			return node, nil
		}
	}
	return nil, fmt.Errorf("schema constraints cannot produce a valid %s placeholder", kind)
}

func templateCompositionCycle(schema *openapi3.Schema, ancestors map[*openapi3.Schema]bool) bool {
	if schema == nil {
		return false
	}
	if ancestors[schema] {
		return true
	}
	ancestors[schema] = true
	defer delete(ancestors, schema)
	for _, reference := range schema.AllOf {
		if templateCompositionCycle(reference.Value, ancestors) {
			return true
		}
	}
	return false
}

func templateObject(schemas []*openapi3.Schema, ancestors map[*openapi3.Schema]bool) (*yaml.Node, error) {
	node := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	properties := make(map[string][]*openapi3.Schema)
	for _, schema := range schemas {
		for name, reference := range schema.Properties {
			properties[name] = append(properties[name], reference.Value)
		}
	}
	names := make([]string, 0, len(properties))
	for name := range properties {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		propertySchemas := properties[name]
		readonly := false
		deprecated := false
		required := false
		for _, schema := range schemas {
			required = required || slices.Contains(schema.Required, name)
		}
		description := ""
		for _, property := range propertySchemas {
			if property == nil {
				return nil, fmt.Errorf("%s: unresolved schema", name)
			}
			readonly = readonly || property.ReadOnly
			deprecated = deprecated || property.Deprecated
			if property.Description != "" {
				description = property.Description
			}
		}
		if readonly || (deprecated && !required) {
			continue
		}
		child, err := templateNode(propertySchemas, ancestors)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		key := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: name, HeadComment: description}
		node.Content = append(node.Content, key, child)
	}
	if len(properties) == 0 {
		var additional []*openapi3.Schema
		for _, schema := range schemas {
			if schema.AdditionalProperties.Schema != nil {
				additional = append(additional, schema.AdditionalProperties.Schema.Value)
			}
		}
		if len(additional) > 0 {
			count := uint64(1)
			for _, schema := range schemas {
				if schema.MinProps > count {
					count = schema.MinProps
				}
			}
			for _, schema := range schemas {
				if schema.MaxProps != nil && *schema.MaxProps < count {
					count = *schema.MaxProps
				}
			}
			if count > 256 {
				return nil, fmt.Errorf("map requires more than 256 placeholder entries")
			}
			for index := uint64(0); index < count; index++ {
				child, err := templateNode(additional, ancestors)
				if err != nil {
					return nil, fmt.Errorf("map value: %w", err)
				}
				key := "replace-me"
				if index > 0 {
					key = fmt.Sprintf("replace-me-%d", index+1)
				}
				node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, child)
			}
		}
	}
	if !templateMatches(node, schemas) {
		return nil, fmt.Errorf("object constraints cannot include every writable property")
	}
	return node, nil
}

func templateArray(schemas []*openapi3.Schema, ancestors map[*openapi3.Schema]bool) (*yaml.Node, error) {
	count := uint64(1)
	var items []*openapi3.Schema
	for _, schema := range schemas {
		if schema.MinItems > count {
			count = schema.MinItems
		}
		if schema.Items != nil {
			items = append(items, schema.Items.Value)
		}
	}
	for _, schema := range schemas {
		if schema.MaxItems != nil && *schema.MaxItems < count {
			count = *schema.MaxItems
		}
	}
	if count > 256 {
		return nil, fmt.Errorf("array requires more than 256 placeholder items")
	}
	node := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for index := uint64(0); index < count; index++ {
		var child *yaml.Node
		var err error
		if len(items) == 0 {
			child, err = templateValue("replace-me")
		} else {
			child, err = templateNode(items, ancestors)
		}
		if err != nil {
			return nil, fmt.Errorf("array item: %w", err)
		}
		node.Content = append(node.Content, child)
	}
	if !templateMatches(node, schemas) {
		return nil, fmt.Errorf("array constraints cannot produce valid placeholder items")
	}
	return node, nil
}

func templateValue(value any) (*yaml.Node, error) {
	node := &yaml.Node{}
	if err := node.Encode(value); err != nil {
		return nil, err
	}
	return node, nil
}

func templateMatches(node *yaml.Node, schemas []*openapi3.Schema) bool {
	var value any
	if err := node.Decode(&value); err != nil {
		return false
	}
	for _, schema := range schemas {
		if schema.VisitJSON(value, openapi3.VisitAsRequest()) != nil {
			return false
		}
	}
	return true
}

func templateStrings(schemas []*openapi3.Schema) ([]string, error) {
	minimum := uint64(0)
	maximum := uint64(16384)
	values := []string{"replace-me", "https://example.com", "user@example.com", "/", "latest", "1", "a", ""}
	for _, schema := range schemas {
		if schema.MinLength > minimum {
			minimum = schema.MinLength
		}
		if schema.MaxLength != nil && *schema.MaxLength < maximum {
			maximum = *schema.MaxLength
		}
	}
	for _, schema := range schemas {
		switch schema.Format {
		case "date-time":
			values = append([]string{"2000-01-01T00:00:00Z"}, values...)
		case "date":
			values = append([]string{"2000-01-01"}, values...)
		case "email":
			values = append([]string{"user@example.com"}, values...)
		case "uri", "url":
			values = append([]string{"https://example.com"}, values...)
		case "uuid":
			values = append([]string{"00000000-0000-4000-8000-000000000000"}, values...)
		case "ipv4":
			values = append([]string{"192.0.2.1"}, values...)
		case "ipv6":
			values = append([]string{"2001:db8::1"}, values...)
		case "hostname":
			values = append([]string{"example.com"}, values...)
		case "byte":
			values = append([]string{"cmVwbGFjZS1tZQ=="}, values...)
		}
		if schema.Pattern != "" {
			expression, err := syntax.Parse(schema.Pattern, syntax.Perl)
			if err != nil {
				return nil, fmt.Errorf("invalid schema pattern: %w", err)
			}
			variants := []int{0, 1, 2, 3, 4, 5, 6, 7}
			if minimum > 7 && minimum <= 16384 {
				variants = append(variants, int(minimum))
			}
			for _, variant := range variants {
				value, err := templatePattern(expression, variant)
				if err != nil {
					continue
				}
				values = append(values, value)
			}
		}
	}
	if minimum > maximum {
		return nil, fmt.Errorf("string length bounds cannot produce a bounded placeholder")
	}
	for _, value := range append([]string{}, values...) {
		length := uint64(utf8.RuneCountInString(value))
		if length < minimum {
			values = append(values, value+strings.Repeat("a", int(minimum-length)))
		}
		if length > maximum {
			values = append(values, string([]rune(value)[:maximum]))
		}
	}
	return values, nil
}

func templatePattern(expression *syntax.Regexp, variant int) (string, error) {
	switch expression.Op {
	case syntax.OpEmptyMatch, syntax.OpBeginLine, syntax.OpEndLine, syntax.OpBeginText, syntax.OpEndText, syntax.OpWordBoundary, syntax.OpNoWordBoundary:
		return "", nil
	case syntax.OpLiteral:
		return string(expression.Rune), nil
	case syntax.OpCharClass:
		for _, preferred := range []rune{'a', '0', '-', '_', '/'} {
			for index := 0; index < len(expression.Rune); index += 2 {
				if preferred >= expression.Rune[index] && preferred <= expression.Rune[index+1] {
					return string(preferred), nil
				}
			}
		}
		if len(expression.Rune) > 0 {
			return string(expression.Rune[0]), nil
		}
	case syntax.OpAnyChar, syntax.OpAnyCharNotNL:
		return "a", nil
	case syntax.OpCapture:
		return templatePattern(expression.Sub[0], variant)
	case syntax.OpAlternate:
		return templatePattern(expression.Sub[variant%len(expression.Sub)], variant)
	case syntax.OpConcat:
		var text strings.Builder
		for _, child := range expression.Sub {
			value, err := templatePattern(child, variant)
			if err != nil {
				return "", err
			}
			text.WriteString(value)
			if text.Len() > 16384 {
				return "", fmt.Errorf("pattern requires an oversized placeholder")
			}
		}
		return text.String(), nil
	case syntax.OpQuest, syntax.OpStar, syntax.OpPlus, syntax.OpRepeat:
		count := variant
		if expression.Op == syntax.OpQuest {
			count %= 2
		}
		if expression.Op == syntax.OpPlus {
			count++
		}
		if expression.Op == syntax.OpRepeat {
			count += expression.Min
			if expression.Max >= 0 && count > expression.Max {
				count = expression.Max
			}
		}
		value, err := templatePattern(expression.Sub[0], variant)
		if err != nil {
			return "", err
		}
		if count > 16384 || len(value)*count > 16384 {
			return "", fmt.Errorf("pattern requires an oversized placeholder")
		}
		return strings.Repeat(value, count), nil
	}
	return "", fmt.Errorf("pattern cannot produce a placeholder")
}

func templateNumbers(schemas []*openapi3.Schema, integer bool) []any {
	minimum, maximum := math.Inf(-1), math.Inf(1)
	for _, schema := range schemas {
		if schema.Min != nil && *schema.Min > minimum {
			minimum = *schema.Min
		}
		if schema.Max != nil && *schema.Max < maximum {
			maximum = *schema.Max
		}
	}
	if integer {
		minimum, maximum = math.Ceil(minimum), math.Floor(maximum)
	}
	if minimum > maximum {
		return nil
	}
	if math.IsInf(minimum, -1) {
		minimum = math.Min(0, maximum)
	}
	return []any{minimum}
}
