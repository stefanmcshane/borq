package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stefanmcshane/borq/api/appapi"
	"gopkg.in/yaml.v3"
)

func newTemplateCommand() *cobra.Command {
	format, example := "yaml", ""
	command := &cobra.Command{Use: "template [OPERATION_OR_SCHEMA]", Short: "Print a configuration template from the embedded API definitions.", Args: cobra.MaximumNArgs(1),
		Long:              "Print every writable field with placeholder values from the embedded API schema. Replace placeholders and remove unused or mutually exclusive fields before applying. Omit the name to list API request templates.",
		Example:           "  borq template PostPlan\n  borq template PutOrganization --example oidc\n  borq apply --template > apps.yaml",
		PersistentPreRunE: func(*cobra.Command, []string) error { return nil },
		RunE: func(command *cobra.Command, arguments []string) error {
			if len(arguments) > 0 {
				return printConfigurationTemplate(command, arguments[0], example, format)
			}
			specification, err := appapi.GetSwagger()
			if err != nil {
				return err
			}
			operations := []string{}
			for _, path := range specification.Paths.InMatchingOrder() {
				for _, operation := range specification.Paths.Value(path).Operations() {
					if operation.RequestBody != nil {
						operations = append(operations, operation.OperationID)
					}
				}
			}
			slices.Sort(operations)
			_, err = fmt.Fprintln(command.OutOrStdout(), strings.Join(operations, "\n"))
			return err
		},
	}
	command.Flags().StringVar(&format, "format", "yaml", "Template output: yaml or json.")
	command.Flags().StringVar(&example, "example", "", "Named request example from the API definition.")
	return command
}

func configurationTemplateSchema(specification *openapi3.T, name string) (*openapi3.Schema, openapi3.Examples, error) {
	if schema := specification.Components.Schemas[name]; schema != nil {
		return schema.Value, nil, nil
	}
	for _, path := range specification.Paths.InMatchingOrder() {
		for _, operation := range specification.Paths.Value(path).Operations() {
			if operation.OperationID != name {
				continue
			}
			if operation.RequestBody != nil && operation.RequestBody.Value != nil {
				media := operation.RequestBody.Value.Content.Get("application/json")
				if media != nil && media.Schema != nil {
					return media.Schema.Value, media.Examples, nil
				}
			}
			return nil, nil, fmt.Errorf("API operation %s has no JSON request body", name)
		}
	}
	return nil, nil, fmt.Errorf("unknown API operation or schema %q; run borq template to list request templates", name)
}

func printConfigurationTemplate(command *cobra.Command, name, example, format string) error {
	if format != "yaml" && format != "json" {
		return errors.New("template format must be yaml or json")
	}
	specification, err := appapi.GetSwagger()
	if err != nil {
		return err
	}
	schema, examples, err := configurationTemplateSchema(specification, name)
	if err != nil {
		return err
	}
	var node *yaml.Node
	if example != "" && example != "full" {
		selected := examples[example]
		if selected == nil || selected.Value == nil {
			available := []string{}
			for name := range examples {
				available = append(available, name)
			}
			slices.Sort(available)
			return fmt.Errorf("unknown template example %q for %s; available examples: %s", example, name, strings.Join(available, ", "))
		}
		node = &yaml.Node{}
		if err := node.Encode(selected.Value.Value); err != nil {
			return err
		}
		node.HeadComment = selected.Value.Summary
	} else {
		node, err = configurationTemplate(schema)
		if err != nil {
			return fmt.Errorf("generate %s template: %w", name, err)
		}
	}
	var value any
	if err := node.Decode(&value); err != nil {
		return err
	}
	if err := schema.VisitJSON(value, openapi3.VisitAsRequest()); err != nil {
		return fmt.Errorf("generated %s template does not match the API schema: %w", name, err)
	}
	if format == "json" {
		encoder := json.NewEncoder(command.OutOrStdout())
		encoder.SetIndent("", "  ")
		return encoder.Encode(value)
	}
	node.HeadComment = "Generated from the embedded Borq API: " + name + ".\nReplace placeholders and remove unused or mutually exclusive fields before applying.\n" + node.HeadComment
	encoder := yaml.NewEncoder(command.OutOrStdout())
	encoder.SetIndent(2)
	if err := encoder.Encode(node); err != nil {
		return err
	}
	return encoder.Close()
}

func selectedConfigurationTemplate(command *cobra.Command) string {
	flag := command.Flags().Lookup("template")
	if flag == nil {
		return ""
	}
	return flag.Value.String()
}

// addConfigurationTemplate makes template inspection independent of execution prerequisites.
func addConfigurationTemplate(command *cobra.Command, schema string) {
	command.Flags().String("template", "", "Print an offline API template; optionally select an example with --template=NAME.")
	command.Flags().Lookup("template").NoOptDefVal = "full"
	if schema == "ApplyManagementConfiguration" {
		command.Flags().Lookup("template").NoOptDefVal = "cloudflare-tailscale"
	}
	command.Flags().String("template-format", "yaml", "Template output: yaml or json (requires --template).")
	originalArgs, originalPreRun, originalRun := command.Args, command.PreRunE, command.RunE
	command.Args = func(command *cobra.Command, arguments []string) error {
		if command.Flags().Changed("template") && selectedConfigurationTemplate(command) == "" {
			return errors.New("use --template for all fields, or --template=NAME for a named example")
		}
		if selectedConfigurationTemplate(command) != "" {
			// Cobra validates required flags after Args, even when RunE only prints a template.
			command.Flags().VisitAll(func(flag *pflag.Flag) { delete(flag.Annotations, cobra.BashCompOneRequiredFlag) })
			return nil
		}
		if command.Flags().Changed("template-format") {
			return errors.New("--template-format requires --template")
		}
		if originalArgs != nil {
			return originalArgs(command, arguments)
		}
		return nil
	}
	command.PreRunE = func(command *cobra.Command, arguments []string) error {
		if selectedConfigurationTemplate(command) != "" {
			return nil
		}
		if originalPreRun != nil {
			return originalPreRun(command, arguments)
		}
		return nil
	}
	command.RunE = func(command *cobra.Command, arguments []string) error {
		if example := selectedConfigurationTemplate(command); example != "" {
			format, _ := command.Flags().GetString("template-format")
			return printConfigurationTemplate(command, schema, example, format)
		}
		return originalRun(command, arguments)
	}
}

func addConfigurationTemplates(root *cobra.Command) {
	requests := map[string]string{
		"management bootstrap": "ApplyManagementConfiguration",
		"management apply":     "ApplyManagementConfiguration",
		"org create":           "PutOrganization",
		"org members update":   "PutOrganizationMember",
		"org roles create":     "CreateOrganizationRole", "org roles update": "PutOrganizationRole",
		"cluster link": "ClusterLinkFile", "space create": "PutSpace",
		"plan": "PostPlan", "apply": "ApplySpaceConfiguration",
		"secrets put": "PutSecret", "management secrets put": "PutManagementSecret",
		"environment-group create": "PublishEnvironmentGroup", "environment-group clone": "CloneEnvironmentGroup",
	}
	for _, prefix := range []string{"am postgres", "addons managed postgres"} {
		requests[prefix+" create"] = "PutPostgres"
		requests[prefix+" apply"] = "ApplySpaceConfiguration"
		requests[prefix+" backup restore"] = "RestorePostgres"
	}
	for _, prefix := range []string{"ac", "addons custom"} {
		requests[prefix+" install"] = "PutCustomChart"
		requests[prefix+" apply"] = "PutCustomChart"
		requests[prefix+" defaults"] = "GetChartDefaults"
	}
	var visit func(*cobra.Command)
	visit = func(command *cobra.Command) {
		path := strings.TrimPrefix(command.CommandPath(), root.Name()+" ")
		if schema := requests[path]; schema != "" {
			addConfigurationTemplate(command, schema)
		}
		for _, child := range command.Commands() {
			visit(child)
		}
	}
	visit(root)
}
