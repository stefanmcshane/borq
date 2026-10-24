package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/stefanmcshane/borq/api/appapi"
	yamlreader "gopkg.in/yaml.v3"
	"sigs.k8s.io/yaml"
)

type customChartOptions struct {
	scope                                                                 applicationScope
	action, name, repository, chart, version, valuesFile, history, search string
	logs, wait                                                            bool
	selector                                                              map[string]string
	limit                                                                 int
	timeout                                                               time.Duration
}

func newCustomChartCommand() *cobra.Command {
	group := &cobra.Command{Use: "custom", Short: "Install and manage custom Helm addons within a space.", Args: cobra.NoArgs}
	for _, action := range []string{"list", "get", "install", "apply", "delete", "defaults", "logs", "metrics", "operation"} {
		options := customChartOptions{action: action}
		command := &cobra.Command{Use: action, Short: "Manage custom addon " + action + ".", Args: cobra.NoArgs,
			RunE: func(command *cobra.Command, arguments []string) error {
				if len(arguments) > 0 {
					options.name = arguments[0]
				}
				return executeCustomChart(command, options)
			},
		}
		if action != "list" && action != "defaults" {
			command.Use += " NAME"
			command.Args = cobra.ExactArgs(1)
		}
		if action == "apply" {
			command.Aliases = []string{"upgrade"}
		}
		if action == "delete" {
			command.Aliases = []string{"uninstall", "remove"}
		}
		addApplicationServerFlag(command, &options.scope)
		if action != "defaults" && action != "operation" {
			command.Flags().StringVar(&options.scope.organization, "organization", "", "Organization owning the space.")
			command.Flags().StringVar(&options.scope.space, "space", "", "Space owning the custom addon.")
		}
		if action == "install" || action == "apply" || action == "defaults" {
			command.Flags().StringVar(&options.repository, "repo", "", "Public Helm repository URL or OCI repository prefix.")
			command.Flags().StringVar(&options.chart, "chart", "", "Published chart name; defaults to NAME when installing.")
			command.Flags().StringVar(&options.version, "version", "", "Exact published chart version.")
			_ = command.MarkFlagRequired("repo")
			_ = command.MarkFlagRequired("version")
			if action == "defaults" {
				_ = command.MarkFlagRequired("chart")
			}
		}
		if action == "install" || action == "apply" {
			command.Flags().StringVarP(&options.valuesFile, "values", "f", "", "YAML values overrides; omitted uses chart defaults.")
			command.Flags().BoolVar(&options.logs, "logs", true, "Collect logs from the custom addon’s selected pods.")
			command.Flags().StringToStringVar(&options.selector, "pod-selector", nil, "Pod template labels to select for collection, as key=value pairs.")
		}
		if action == "install" || action == "apply" || action == "delete" {
			command.Flags().BoolVar(&options.wait, "wait", true, "Wait for the durable Helm operation to finish.")
			command.Flags().DurationVar(&options.timeout, "timeout", 10*time.Minute, "Maximum local wait time; execution continues on the server.")
		}
		if action == "logs" || action == "metrics" {
			command.Flags().StringVar(&options.history, "range", "1h", "History window: 30m, 1h, 12h, or 3d.")
		}
		if action == "logs" {
			command.Flags().StringVar(&options.search, "search", "", "Literal substring to find in log output.")
			command.Flags().IntVar(&options.limit, "limit", 200, "Maximum newest log lines, from 1 to 1000.")
		}
		group.AddCommand(command)
	}
	return group
}

func executeCustomChart(command *cobra.Command, options customChartOptions) error {
	if options.wait && options.timeout <= 0 {
		return errors.New("--timeout must be greater than zero")
	}
	client, err := applicationClient(options.scope.server)
	if err != nil {
		return err
	}
	organization, space := options.scope.organization, options.scope.space
	switch options.action {
	case "defaults":
		response, err := client.GetChartDefaultsWithResponse(command.Context(), appapi.ChartInput{Repository: options.repository, Name: options.chart, Version: options.version})
		if err != nil {
			return err
		}
		if response.JSON200 == nil {
			return applicationFailure(response.StatusCode(), response.Body)
		}
		_, err = fmt.Fprint(command.OutOrStdout(), response.JSON200.Values)
		return err
	case "list":
		response, err := client.ListCustomChartsWithResponse(command.Context(), organization, space)
		if err != nil {
			return err
		}
		if response.JSON200 == nil {
			return applicationFailure(response.StatusCode(), response.Body)
		}
		return printApplicationValue(command.OutOrStdout(), *response.JSON200)
	case "get":
		response, err := client.GetCustomChartWithResponse(command.Context(), organization, space, options.name)
		if err != nil {
			return err
		}
		if response.JSON200 == nil {
			return applicationFailure(response.StatusCode(), response.Body)
		}
		return printApplicationValue(command.OutOrStdout(), *response.JSON200)
	case "operation":
		response, err := client.GetCustomChartOperationWithResponse(command.Context(), options.name)
		if err != nil {
			return err
		}
		if response.JSON200 == nil {
			return applicationFailure(response.StatusCode(), response.Body)
		}
		return printApplicationValue(command.OutOrStdout(), *response.JSON200)
	case "metrics":
		history := appapi.GetCustomChartMetricsParamsRange(options.history)
		response, err := client.GetCustomChartMetricsWithResponse(command.Context(), organization, space, options.name, &appapi.GetCustomChartMetricsParams{Range: &history})
		if err != nil {
			return err
		}
		if response.JSON200 == nil {
			return applicationFailure(response.StatusCode(), response.Body)
		}
		return printApplicationValue(command.OutOrStdout(), *response.JSON200)
	case "logs":
		history := appapi.GetCustomChartLogsParamsRange(options.history)
		response, err := client.GetCustomChartLogsWithResponse(command.Context(), organization, space, options.name, &appapi.GetCustomChartLogsParams{Range: &history, Search: &options.search, Limit: &options.limit})
		if err != nil {
			return err
		}
		if response.JSON200 == nil {
			return applicationFailure(response.StatusCode(), response.Body)
		}
		return printApplicationValue(command.OutOrStdout(), *response.JSON200)
	case "install", "apply", "delete":
		return executeCustomChartChange(command, client, options)
	}
	return errors.New("unsupported custom addon command")
}

// The generated client uses a pointer for its HTTP connection state.
func executeCustomChartChange(command *cobra.Command, client *appapi.ClientWithResponses, options customChartOptions) error {
	var customChart appapi.CustomChart
	if options.action == "delete" {
		response, err := client.DeleteCustomChartWithResponse(command.Context(), options.scope.organization, options.scope.space, options.name)
		if err != nil {
			return err
		}
		if response.JSON202 == nil {
			return applicationFailure(response.StatusCode(), response.Body)
		}
		customChart = *response.JSON202
	} else {
		values, err := readCustomChartValues(options.valuesFile)
		if err != nil {
			return err
		}
		if options.chart == "" {
			options.chart = options.name
		}
		body := appapi.CustomChartInput{Repository: options.repository, Chart: options.chart, Version: options.version, Values: &values, Logs: &options.logs}
		if len(options.selector) > 0 {
			body.PodSelector = &options.selector
		}
		// The generated query model uses a pointer to distinguish omission from false.
		apply := options.action == "apply"
		response, err := client.PutCustomChartWithResponse(command.Context(), options.scope.organization, options.scope.space, options.name, &appapi.PutCustomChartParams{Apply: &apply}, body)
		if err != nil {
			return err
		}
		if response.JSON202 == nil {
			return applicationFailure(response.StatusCode(), response.Body)
		}
		customChart = *response.JSON202
	}
	if !options.wait {
		return printApplicationValue(command.OutOrStdout(), customChart)
	}
	ctx, cancel := context.WithTimeout(command.Context(), options.timeout)
	defer cancel()
	return waitCustomChartOperation(ctx, command, client, customChart.Operation)
}

// Cobra and the generated client require pointers to write output and make requests.
func waitCustomChartOperation(ctx context.Context, command *cobra.Command, client *appapi.ClientWithResponses, id string) error {
	lastMessage := ""
	for {
		response, err := client.GetCustomChartOperationWithResponse(ctx, id)
		if err != nil {
			return err
		}
		if response.JSON200 == nil {
			return applicationFailure(response.StatusCode(), response.Body)
		}
		operation := *response.JSON200
		if operation.Message != lastMessage {
			if _, err := fmt.Fprintln(command.ErrOrStderr(), operation.Message); err != nil {
				return err
			}
			lastMessage = operation.Message
		}
		if operation.Status == appapi.Failed {
			return fmt.Errorf("custom addon operation %s failed: %s", id, operation.Message)
		}
		if operation.Status == appapi.Succeeded {
			return printApplicationValue(command.OutOrStdout(), operation)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func readCustomChartValues(path string) (map[string]any, error) {
	if path == "" {
		return map[string]any{}, nil
	}
	source, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read custom chart values: %w", err)
	}
	decoder := yamlreader.NewDecoder(bytes.NewReader(source))
	var document any
	if err := decoder.Decode(&document); err != nil {
		return nil, errors.New("custom chart values must contain one YAML object")
	}
	if err := decoder.Decode(&document); err != io.EOF {
		return nil, errors.New("custom chart values must contain exactly one YAML document")
	}
	var values map[string]any
	if err := yaml.UnmarshalStrict(source, &values); err != nil || values == nil {
		return nil, errors.New("custom chart values must be a YAML object with unique keys")
	}
	return values, nil
}
