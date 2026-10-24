package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"github.com/stefanmcshane/borq/api/appapi"
)

func printComponentDiff(writer io.Writer, diff appapi.ComponentDiff, verbose bool) error {
	if len(diff.Components) == 0 {
		_, err := fmt.Fprintln(writer, "No target component changes.")
		return err
	}
	for _, component := range diff.Components {
		var message string
		switch component.Action {
		case "install":
			message = fmt.Sprintf("Missing target component: %s (%s)", component.Name, component.DesiredVersion)
		case "none":
			message = fmt.Sprintf("Already installed: %s (%s)", component.Name, component.InstalledVersion)
		case "conflict":
			message = fmt.Sprintf("Component conflict: %s: %s", component.Name, component.Reason)
		default:
			message = fmt.Sprintf("Component %s needs %s (%s)", component.Name, component.Action, component.DesiredVersion)
		}
		if _, err := fmt.Fprintln(writer, message); err != nil {
			return err
		}
		if verbose && component.ValuesDiff != "" {
			if _, err := fmt.Fprintln(writer, strings.TrimRight(component.ValuesDiff, "\n")); err != nil {
				return err
			}
		}
	}
	return nil
}

// Cobra owns output destinations; machine output is explicit so status records
// cannot be mistaken for the human-readable preview of target changes.
func printClusterLinkValue(command *cobra.Command, options clusterLinkOptions, value any) error {
	switch options.output {
	case "json":
		return json.NewEncoder(command.OutOrStdout()).Encode(value)
	case "yaml":
		return printApplicationValue(command.OutOrStdout(), value)
	default:
		return nil
	}
}
