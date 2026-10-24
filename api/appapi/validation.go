package appapi

import (
	"errors"

	"github.com/getkin/kin-openapi/openapi3"
)

// SchemaValidationMessage returns contract-authored guidance without exposing
// submitted values or diagnostic details. An empty result leaves fallback text
// to the caller.
func SchemaValidationMessage(err error) string {
	var schemaError *openapi3.SchemaError
	if !errors.As(err, &schemaError) || schemaError.Schema == nil {
		return ""
	}
	message, _ := schemaError.Schema.Extensions["x-borq-validation-message"].(string)
	return message
}
