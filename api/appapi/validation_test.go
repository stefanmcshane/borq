package appapi

import (
	"errors"
	"fmt"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/matryer/is"
)

// TestSchemaValidationMessage restricts public guidance to string annotations
// authored in the contract, even when diagnostics include submitted secrets.
func TestSchemaValidationMessage(t *testing.T) {
	check := is.New(t)
	for _, input := range []struct {
		annotation any
		message    string
	}{
		{"Use supported PostgreSQL fields.", "Use supported PostgreSQL fields."},
		{nil, ""},
		{42, ""},
	} {
		schema := openapi3.NewObjectSchema()
		schema.Extensions = map[string]any{"x-borq-validation-message": input.annotation}
		err := &openapi3.SchemaError{Schema: schema, Value: "private-token", Reason: "private-token"}
		check.Equal(SchemaValidationMessage(fmt.Errorf("private-token: %w", err)), input.message)
	}
	check.Equal(SchemaValidationMessage(errors.New("private-token")), "")
	check.Equal(SchemaValidationMessage(&openapi3.SchemaError{Reason: "private-token"}), "")
}
