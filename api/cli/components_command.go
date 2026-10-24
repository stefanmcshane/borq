package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"github.com/stefanmcshane/borq/api/appapi"
)

type clusterComponentOptions struct {
	scope         applicationScope
	wait, upgrade bool
	timeout       time.Duration
}

func newClusterComponentsCommands() []*cobra.Command {
	commands := []*cobra.Command{}
	for _, action := range []string{"components", "diff", "reconcile", "upgrade"} {
		options := clusterComponentOptions{wait: true, timeout: 30 * time.Minute}
		command := &cobra.Command{Use: action + " [NAME]", Short: "Inspect or reconcile managed deployment target components.", Args: cobra.MaximumNArgs(1),
			RunE: func(command *cobra.Command, arguments []string) error {
				name, err := clusterArgument(arguments, options.scope.cluster)
				if err != nil {
					return err
				}
				return executeClusterComponents(command, options, action, name)
			},
		}
		addOrganizationScopeFlags(command, &options.scope)
		if action == "reconcile" || action == "upgrade" {
			addClusterWaitFlags(command, &options.wait, &options.timeout)
		}
		if action == "diff" {
			command.Short = "Compare target components without installing or enqueueing anything."
			command.Flags().BoolVar(&options.upgrade, "upgrade", false, "Show changes that an explicit upgrade would apply.")
		}
		commands = append(commands, command)
	}
	return commands
}

// Cobra requires pointers to populate command-local wait flags.
func addClusterWaitFlags(command *cobra.Command, wait *bool, timeout *time.Duration) {
	command.Flags().BoolVar(wait, "wait", true, "Wait for server-owned component reconciliation; false returns after enqueueing.")
	command.Flags().DurationVar(timeout, "timeout", 30*time.Minute, "Maximum local wait time; server work continues after timeout.")
}

func executeClusterComponents(command *cobra.Command, options clusterComponentOptions, action, name string) error {
	if (action == "reconcile" || action == "upgrade") && options.wait && options.timeout <= 0 {
		return errors.New("--timeout must be greater than zero")
	}
	client, err := applicationClient(options.scope.server)
	if err != nil {
		return err
	}
	var result appapi.ComponentInstallation
	switch action {
	case "components":
		response, err := client.GetClusterComponentsWithResponse(command.Context(), options.scope.organization, name)
		if err != nil {
			return err
		}
		if response.JSON200 == nil {
			return applicationFailure(response.StatusCode(), response.Body)
		}
		result = *response.JSON200
	case "diff":
		response, err := client.DiffClusterComponentsWithResponse(command.Context(), options.scope.organization, name, &appapi.DiffClusterComponentsParams{Upgrade: &options.upgrade})
		if err != nil {
			return err
		}
		if response.JSON200 == nil {
			return applicationFailure(response.StatusCode(), response.Body)
		}
		return printApplicationValue(command.OutOrStdout(), response.JSON200)
	case "reconcile":
		response, err := client.ReconcileClusterComponentsWithResponse(command.Context(), options.scope.organization, name)
		if err != nil {
			return err
		}
		if response.JSON200 == nil {
			return applicationFailure(response.StatusCode(), response.Body)
		}
		result = *response.JSON200
	case "upgrade":
		response, err := client.UpgradeClusterComponentsWithResponse(command.Context(), options.scope.organization, name)
		if err != nil {
			return err
		}
		if response.JSON200 == nil {
			return applicationFailure(response.StatusCode(), response.Body)
		}
		result = *response.JSON200
	default:
		return errors.New("unknown component operation")
	}
	var waitErr error
	if action != "components" && options.wait {
		ctx, cancel := context.WithTimeout(command.Context(), options.timeout)
		defer cancel()
		result, waitErr = waitForClusterComponents(ctx, command, client, options.scope, name, result)
	}
	if err := printApplicationValue(command.OutOrStdout(), result); err != nil {
		return err
	}
	return waitErr
}

// The command and generated client pointers preserve Cobra output destinations
// and the HTTP client's configured transport while polling the saved run.
func waitForClusterComponents(ctx context.Context, command *cobra.Command, client *appapi.ClientWithResponses, scope applicationScope, name string, initial appapi.ComponentInstallation) (appapi.ComponentInstallation, error) {
	result := initial
	runID := initial.ID
	lastStatus := ""
	diffPrinted := false
	for {
		if result.Status != "" {
			if runID != "" && result.ID != runID {
				return result, fmt.Errorf("component run %s was replaced by %s; inspect cluster components", runID, result.ID)
			}
			if runID == "" {
				runID = result.ID
			}
			if result.Status != lastStatus {
				_, _ = fmt.Fprintf(command.ErrOrStderr(), "Components %s: %s\n", result.Status, result.Message)
				lastStatus = result.Status
			}
			if !diffPrinted && len(result.Diff.Components) > 0 {
				if err := printComponentDiff(command.ErrOrStderr(), result.Diff, false); err != nil {
					return result, err
				}
				diffPrinted = true
			}
			switch result.Status {
			case "ready", "disabled":
				return result, nil
			case "failed":
				return result, fmt.Errorf("component reconciliation failed: %s", result.Message)
			case "queued", "diffing", "installing":
			default:
				return result, fmt.Errorf("unknown component reconciliation status %q", result.Status)
			}
			select {
			case <-ctx.Done():
				return result, ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
		}
		response, err := client.GetClusterComponentsWithResponse(ctx, scope.organization, name)
		if err != nil {
			return result, err
		}
		if response.JSON200 == nil {
			return result, applicationFailure(response.StatusCode(), response.Body)
		}
		result = *response.JSON200
		if result.Status == "" {
			return result, errors.New("component status response is empty")
		}
	}
}
