package cli

import (
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/matryer/is"
	"github.com/stefanmcshane/borq/api/appapi"
	"gopkg.in/yaml.v3"
)

// TestConfigurationTemplateRequestSchemas catches templates becoming invalid when
// any embedded API request definition gains fields or constraints.
func TestConfigurationTemplateRequestSchemas(t *testing.T) {
	specification, err := appapi.GetSwagger()
	is.New(t).NoErr(err)
	for path, item := range specification.Paths.Map() {
		for method, operation := range item.Operations() {
			if operation.RequestBody == nil {
				continue
			}
			body := operation.RequestBody.Value.Content.Get("application/json")
			if body == nil || body.Schema == nil {
				continue
			}
			t.Run(method+" "+path, func(t *testing.T) {
				assert := is.New(t)
				schema := body.Schema.Value
				node, err := configurationTemplate(schema)
				assert.NoErr(err)
				if err != nil {
					return
				}
				encoded, err := yaml.Marshal(node)
				assert.NoErr(err)
				var request any
				assert.NoErr(yaml.Unmarshal(encoded, &request))
				assert.NoErr(schema.VisitJSON(request, openapi3.VisitAsRequest()))
				for name, property := range schema.Properties {
					if property.Value.ReadOnly {
						continue
					}
					object, ok := request.(map[string]any)
					assert.True(ok)
					if ok {
						_, present := object[name]
						assert.True(present)
					}
				}
			})
		}
	}
}

// TestConfigurationTemplateConstraints protects placeholder validity and visible
// documentation for the constraints used by Borq's configuration schemas.
func TestConfigurationTemplateConstraints(t *testing.T) {
	for _, test := range []struct {
		name       string
		definition string
		contains   []string
	}{
		{"fields", `{"type":"object","required":["name","readonly"],"additionalProperties":false,"properties":{"name":{"type":"string","description":"Edit the name.","minLength":2,"maxLength":4,"pattern":"^[a-z]+$"},"readonly":{"type":"string","readOnly":true},"secret":{"type":"string","writeOnly":true},"map":{"type":"object","additionalProperties":{"type":"string"}},"free":{"type":"object"}}}`, []string{"# Edit the name.", "secret:", "replace-me:", "free: {}"}},
		{"defaults", `{"type":"object","properties":{"choice":{"type":"string","enum":["blue","red"],"default":"red"},"time":{"type":"string","format":"date-time"},"revision":{"type":"integer","minimum":3,"maximum":6}}}`, []string{"choice: red", "2000-01-01T00:00:00Z", "revision: 3"}},
		{"allof", `{"allOf":[{"type":"object","properties":{"left":{"type":"string"},"shared":{"type":"integer","minimum":3}}},{"type":"object","properties":{"right":{"type":"boolean"},"shared":{"type":"integer","maximum":5}}}]}`, []string{"left:", "right:", "shared: 3"}},
		{"array", `{"type":"array","minItems":2,"items":{"type":"object","required":["name"],"properties":{"name":{"type":"string"}}}}`, []string{"- name:"}},
		{"zeroarray", `{"type":"array","maxItems":0,"items":{"type":"string"}}`, []string{"[]"}},
		{"mapbound", `{"type":"object","minProperties":2,"additionalProperties":{"type":"string"}}`, []string{"replace-me-2:"}},
		{"composedpattern", `{"allOf":[{"type":"string","pattern":"^[0-9]+$"},{"type":"string","minLength":30}]}`, nil},
		{"longpattern", `{"type":"string","minLength":30,"pattern":"^[0-9]+$"}`, nil},
		{"pattern", `{"type":"string","pattern":"^hello-(cat|dog)-[0-9]{2,4}$"}`, []string{"hello-cat-00"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := is.New(t)
			var schema openapi3.Schema
			assert.NoErr(schema.UnmarshalJSON([]byte(test.definition)))
			node, err := configurationTemplate(&schema)
			assert.NoErr(err)
			if err != nil {
				return
			}
			encoded, err := yaml.Marshal(node)
			assert.NoErr(err)
			var value any
			assert.NoErr(yaml.Unmarshal(encoded, &value))
			assert.NoErr(schema.VisitJSON(value, openapi3.VisitAsRequest()))
			for _, text := range test.contains {
				assert.True(strings.Contains(string(encoded), text))
			}
			if test.name == "fields" {
				assert.True(!strings.Contains(string(encoded), "readonly:"))
			}
		})
	}
}

// TestConfigurationTemplateRecursion terminates expandable recursive objects and
// rejects mandatory infinite structures rather than printing invalid YAML.
func TestConfigurationTemplateRecursion(t *testing.T) {
	assert := is.New(t)
	schema := openapi3.NewObjectSchema()
	schema.Properties["child"] = &openapi3.SchemaRef{Value: schema}
	node, err := configurationTemplate(schema)
	assert.NoErr(err)
	encoded, err := yaml.Marshal(node)
	assert.NoErr(err)
	var value map[string]any
	assert.NoErr(yaml.Unmarshal(encoded, &value))
	assert.Equal(value["child"], map[string]any{})
	assert.True(strings.Contains(string(encoded), "Recursive structure"))
	composition := &openapi3.Schema{}
	composition.AllOf = openapi3.SchemaRefs{&openapi3.SchemaRef{Value: composition}}
	_, err = configurationTemplate(composition)
	assert.True(err != nil)
	assert.True(strings.Contains(err.Error(), "recursive schema composition"))
	schema.Required = []string{"child"}
	_, err = configurationTemplate(schema)
	assert.True(err != nil)
	assert.True(strings.Contains(err.Error(), "recursive schema"))
}

// TestConfigurationTemplateRejectsImpossibleConstraints keeps errors explicit
// when a finite schema-valid placeholder cannot be generated.
func TestConfigurationTemplateRejectsImpossibleConstraints(t *testing.T) {
	for _, definition := range []string{
		`{"type":"string","minLength":5,"maxLength":2}`,
		`{"type":"integer","minimum":2,"maximum":1}`,
		`{"type":"array","minItems":300,"items":{"type":"string"}}`,
	} {
		var schema openapi3.Schema
		assert := is.New(t)
		assert.NoErr(schema.UnmarshalJSON([]byte(definition)))
		_, err := configurationTemplate(&schema)
		assert.True(err != nil)
	}
}
