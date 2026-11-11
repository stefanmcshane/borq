package cli

import (
	"encoding/json"
	"fmt"
	"github.com/spf13/cobra"
	"github.com/stefanmcshane/borq/api/appapi"
	"os"
	"sigs.k8s.io/yaml"
)

func newSecretsCommand() *cobra.Command {
	command := &cobra.Command{Use: "secrets", Aliases: []string{"secret"}, Short: "Manage typed JSON secrets in a space."}
	for _, verb := range []string{"list", "get", "put", "delete"} {
		options := environmentGroupOptions{}
		operation := verb
		use := verb + " NAME"
		args := cobra.ExactArgs(1)
		if verb == "list" {
			use = verb
			args = cobra.NoArgs
		}
		child := &cobra.Command{Use: use, Short: fmt.Sprintf("%s space secrets.", verb), Args: args, RunE: func(command *cobra.Command, args []string) error {
			if len(args) > 0 {
				options.name = args[0]
			}
			return executeSecret(command, operation, options)
		}}
		addEnvironmentGroupScopeFlags(child, &options)
		if verb == "get" {
			child.Flags().StringVar(&options.version, "version", "latest", "Secret-group version to read.")
		}
		if verb == "put" {
			child.Flags().StringVarP(&options.file, "file", "f", "", "JSON or YAML document with type and data keys.")
			_ = child.MarkFlagRequired("file")
		}
		if verb == "delete" {
			child.Flags().IntVar(&options.baseVersion, "version", 0, "Require this current version before deleting.")
		}
		command.AddCommand(child)
	}
	return command
}

func executeSecret(command *cobra.Command, operation string, options environmentGroupOptions) error {
	client, err := applicationClient(options.scope.server)
	if err != nil {
		return err
	}
	ctx := command.Context()
	org, space := options.scope.organization, options.scope.space
	switch operation {
	case "list":
		response, err := client.ListSecretsWithResponse(ctx, org, space)
		if err != nil {
			return err
		}
		if response.JSON200 == nil {
			return applicationFailure(response.StatusCode(), response.Body)
		}
		return printApplicationValue(command.OutOrStdout(), response.JSON200)
	case "get":
		response, err := client.GetSecretVersionWithResponse(ctx, org, space, options.name, options.version)
		if err != nil {
			return err
		}
		if response.JSON200 == nil {
			return applicationFailure(response.StatusCode(), response.Body)
		}
		return printApplicationValue(command.OutOrStdout(), response.JSON200)
	case "put":
		source, err := os.ReadFile(options.file)
		if err != nil {
			return err
		}
		data, err := yaml.YAMLToJSONStrict(source)
		if err != nil {
			return fmt.Errorf("invalid secret document")
		}
		var body appapi.PutSecretInput
		specification, err := appapi.GetSwagger()
		if err != nil {
			return err
		}
		var value any
		if err = json.Unmarshal(data, &value); err != nil {
			return fmt.Errorf("invalid secret document")
		}
		if err = specification.Components.Schemas["PutSecretInput"].Value.VisitJSON(value); err != nil {
			return fmt.Errorf("secret document requires a supported type and a string-valued data object")
		}
		if err = json.Unmarshal(data, &body); err != nil {
			return fmt.Errorf("invalid secret document")
		}
		response, err := client.PutSecretWithResponse(ctx, org, space, options.name, body)
		if err != nil {
			return err
		}
		if response.JSON200 == nil {
			return applicationFailure(response.StatusCode(), response.Body)
		}
		return printApplicationValue(command.OutOrStdout(), response.JSON200)
	case "delete":
		if options.baseVersion < 0 {
			return fmt.Errorf("--version must be positive")
		}
		params := appapi.DeleteSecretParams{}
		// The generated client uses a pointer to distinguish omission from a version guard.
		if options.baseVersion > 0 {
			params.Version = &options.baseVersion
		}
		response, err := client.DeleteSecretWithResponse(ctx, org, space, options.name, &params)
		if err != nil {
			return err
		}
		if response.JSON200 == nil {
			return applicationFailure(response.StatusCode(), response.Body)
		}
		return printApplicationValue(command.OutOrStdout(), response.JSON200)
	}
	return nil
}
