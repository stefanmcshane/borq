package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"github.com/stefanmcshane/borq/api/appapi"
	yamlreader "gopkg.in/yaml.v3"
	"sigs.k8s.io/yaml"
)

type environmentGroupOptions struct {
	scope                       applicationScope
	name, version, source, file string
	baseVersion                 int
}

func newEnvironmentGroupCommand() *cobra.Command {
	group := &cobra.Command{Use: "environment-group", Aliases: []string{"environment-groups", "env"}, Short: "Create and inspect named environment groups within a space."}
	listOptions := environmentGroupOptions{}
	list := &cobra.Command{Use: "list", Short: "List the latest published environment group versions.", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, arguments []string) error {
			return executeEnvironmentGroupList(command, listOptions)
		},
	}
	addEnvironmentGroupScopeFlags(list, &listOptions)
	getOptions := environmentGroupOptions{}
	get := &cobra.Command{Use: "get NAME", Short: "Read an environment group version with its complete values.", Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			getOptions.name = arguments[0]
			return executeEnvironmentGroupGet(command, getOptions)
		},
	}
	addEnvironmentGroupScopeFlags(get, &getOptions)
	get.Flags().StringVar(&getOptions.version, "version", "latest", "Published version to read, or latest.")
	publishOptions := environmentGroupOptions{}
	publish := &cobra.Command{Use: "create NAME", Aliases: []string{"publish"}, Short: "Create an environment group or its next version from a YAML file.", Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			publishOptions.name = arguments[0]
			return executeEnvironmentGroupPublish(command, publishOptions)
		},
	}
	addEnvironmentGroupScopeFlags(publish, &publishOptions)
	publish.Flags().StringVarP(&publishOptions.file, "file", "f", "", "YAML file containing values: [{name: VARIABLE, value: value}].")
	publish.Flags().IntVar(&publishOptions.baseVersion, "base-version", -1, "Require this current version; zero requires a new environment group.")
	_ = publish.MarkFlagRequired("file")
	cloneOptions := environmentGroupOptions{}
	clone := &cobra.Command{Use: "clone NAME", Short: "Create an environment group by cloning an existing version.", Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			cloneOptions.name = arguments[0]
			return executeEnvironmentGroupClone(command, cloneOptions)
		},
	}
	addEnvironmentGroupScopeFlags(clone, &cloneOptions)
	clone.Flags().StringVar(&cloneOptions.source, "from", "", "Existing environment group to copy.")
	clone.Flags().StringVar(&cloneOptions.version, "version", "latest", "Source version to clone, or latest.")
	_ = clone.MarkFlagRequired("from")
	group.AddCommand(list, get, publish, clone)
	return group
}

// The pointer lets Cobra populate command-local options without shared state.
func addEnvironmentGroupScopeFlags(command *cobra.Command, options *environmentGroupOptions) {
	addApplicationServerFlag(command, &options.scope)
	command.Flags().StringVar(&options.scope.organization, "organization", "", "Organization owning the space.")
	command.Flags().StringVar(&options.scope.space, "space", "", "Deployment space owning these configs.")
}

func executeEnvironmentGroupList(command *cobra.Command, options environmentGroupOptions) error {
	client, err := applicationClient(options.scope.server)
	if err != nil {
		return err
	}
	response, err := client.ListEnvironmentGroupsWithResponse(command.Context(), options.scope.organization, options.scope.space)
	if err != nil {
		return err
	}
	if response.JSON200 == nil {
		return applicationFailure(response.StatusCode(), response.Body)
	}
	return printApplicationValue(command.OutOrStdout(), *response.JSON200)
}

func executeEnvironmentGroupGet(command *cobra.Command, options environmentGroupOptions) error {
	client, err := applicationClient(options.scope.server)
	if err != nil {
		return err
	}
	response, err := client.GetEnvironmentGroupVersionWithResponse(command.Context(), options.scope.organization, options.scope.space, options.name, options.version)
	if err != nil {
		return err
	}
	if response.JSON200 == nil {
		return applicationFailure(response.StatusCode(), response.Body)
	}
	return printApplicationValue(command.OutOrStdout(), response.JSON200)
}

func executeEnvironmentGroupPublish(command *cobra.Command, options environmentGroupOptions) error {
	source, err := os.ReadFile(options.file)
	if err != nil {
		return fmt.Errorf("read environment group file: %w", err)
	}
	body, err := decodeEnvironmentGroup(source)
	if err != nil {
		return err
	}
	if command.Flags().Changed("base-version") && options.baseVersion < 0 {
		return errors.New("--base-version must be zero or greater")
	}
	if options.baseVersion >= 0 {
		body.BaseVersion = &options.baseVersion
	}
	client, err := applicationClient(options.scope.server)
	if err != nil {
		return err
	}
	response, err := client.PublishEnvironmentGroupWithResponse(command.Context(), options.scope.organization, options.scope.space, options.name, body)
	if err != nil {
		return err
	}
	if response.JSON201 == nil {
		return applicationFailure(response.StatusCode(), response.Body)
	}
	return printApplicationValue(command.OutOrStdout(), response.JSON201)
}

func executeEnvironmentGroupClone(command *cobra.Command, options environmentGroupOptions) error {
	client, err := applicationClient(options.scope.server)
	if err != nil {
		return err
	}
	response, err := client.CloneEnvironmentGroupWithResponse(command.Context(), options.scope.organization, options.scope.space, options.name,
		appapi.CloneEnvironmentGroupInput{SourceName: options.source, SourceVersion: &options.version})
	if err != nil {
		return err
	}
	if response.JSON201 == nil {
		return applicationFailure(response.StatusCode(), response.Body)
	}
	return printApplicationValue(command.OutOrStdout(), response.JSON201)
}

func decodeEnvironmentGroup(source []byte) (appapi.PublishEnvironmentGroupInput, error) {
	decoder := yamlreader.NewDecoder(bytes.NewReader(source))
	var document any
	if err := decoder.Decode(&document); err != nil {
		return appapi.PublishEnvironmentGroupInput{}, errors.New("environment group file must contain one YAML document")
	}
	if err := decoder.Decode(&document); err != io.EOF {
		return appapi.PublishEnvironmentGroupInput{}, errors.New("environment group file must contain exactly one YAML document")
	}
	jsonSource, err := yaml.YAMLToJSONStrict(source)
	if err != nil {
		return appapi.PublishEnvironmentGroupInput{}, errors.New("environment group file must contain unique YAML keys")
	}
	var value any
	if err := json.Unmarshal(jsonSource, &value); err != nil {
		return appapi.PublishEnvironmentGroupInput{}, errors.New("invalid environment group file")
	}
	specification, err := appapi.GetSwagger()
	if err != nil {
		return appapi.PublishEnvironmentGroupInput{}, err
	}
	if err := specification.Components.Schemas["PublishEnvironmentGroupInput"].Value.VisitJSON(value); err != nil {
		return appapi.PublishEnvironmentGroupInput{}, errors.New("environment group file does not match the OpenAPI schema")
	}
	var body appapi.PublishEnvironmentGroupInput
	if err := json.Unmarshal(jsonSource, &body); err != nil {
		return appapi.PublishEnvironmentGroupInput{}, errors.New("invalid environment group file")
	}
	return body, nil
}
