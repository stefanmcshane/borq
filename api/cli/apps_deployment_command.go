package cli

import "github.com/spf13/cobra"

func newAppsHistoryCommand() *cobra.Command {
	scope := applicationScope{}
	command := &cobra.Command{
		Use:   "history APP",
		Short: "Print the application's twenty most recent deployments as YAML.",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			return executeAppsHistory(command, scope, arguments[0])
		},
	}
	addApplicationServerFlag(command, &scope)
	command.Flags().StringVar(&scope.organization, "organization", "", "Organization owning the space.")
	command.Flags().StringVar(&scope.space, "space", "", "Space whose application history to read.")
	return command
}

func newAppsCancelCommand() *cobra.Command {
	scope := applicationScope{}
	command := &cobra.Command{
		Use:   "cancel OPERATION_ID",
		Short: "Cancel a deployment, retaining accepted settings and applied resources.",
		Long: "Cancel a deployment by its operation ID from apps history or apply output.\n" +
			"Returns the current operation as YAML without waiting for cleanup; running work may report canceling.\n" +
			"Completed operations are returned unchanged. Accepted settings and applied resources are retained.",
		Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			return executeAppsCancel(command, scope, arguments[0])
		},
	}
	addApplicationServerFlag(command, &scope)
	return command
}

// Cobra requires a command pointer to retain the invocation's output destination and context.
func executeAppsHistory(command *cobra.Command, scope applicationScope, application string) error {
	client, err := applicationClient(scope.server)
	if err != nil {
		return err
	}
	response, err := client.ListAppDeploymentsWithResponse(command.Context(), scope.organization, scope.space, application)
	if err != nil {
		return err
	}
	if response.JSON200 == nil {
		return applicationFailure(response.StatusCode(), response.Body)
	}
	return printApplicationValue(command.OutOrStdout(), *response.JSON200)
}

// Cobra requires a command pointer to retain the invocation's output destination and context.
func executeAppsCancel(command *cobra.Command, scope applicationScope, operation string) error {
	client, err := applicationClient(scope.server)
	if err != nil {
		return err
	}
	response, err := client.CancelOperationWithResponse(command.Context(), operation)
	if err != nil {
		return err
	}
	if response.JSON200 == nil {
		return applicationFailure(response.StatusCode(), response.Body)
	}
	return printApplicationValue(command.OutOrStdout(), *response.JSON200)
}
