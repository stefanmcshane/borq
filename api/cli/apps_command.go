package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/stefanmcshane/borq/api/appapi"
	yamlreader "gopkg.in/yaml.v3"
	"sigs.k8s.io/yaml"
)

type applicationScope struct {
	server, apiUrl, organization, space, cluster string
}

type deploymentOptions struct {
	scope                  applicationScope
	file, planFile, planID string
	manifests, wait        bool
	timeout                time.Duration
}

type savedApplicationPlan struct {
	Server       string `json:"server"`
	Organization string `json:"organization"`
	Space        string `json:"space"`
	SourceHash   string `json:"sourceHash"`
	PlanID       string `json:"planId"`
	// Automatic distinguishes an implicit apply plan from an explicitly reviewed plan.
	Automatic bool `json:"automatic,omitempty"`
}

func newApplicationCommands() []*cobra.Command {
	return []*cobra.Command{newOrganizationCommand(), newClusterCommand(), newSpaceCommand(), newAppsCommand(), newPlanCommand(), newApplyCommand()}
}

func newPlanCommand() *cobra.Command {
	options := deploymentOptions{}
	command := &cobra.Command{Use: "plan", Short: "Save a deployment plan and print the complete expanded apps.", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, arguments []string) error {
			return executeApplicationPlan(command, options)
		},
	}
	addDeploymentFlags(command, &options)
	command.Flags().BoolVar(&options.manifests, "manifests", false, "Print the full rendered Kubernetes manifests.")
	return command
}

func newApplyCommand() *cobra.Command {
	options := deploymentOptions{}
	command := &cobra.Command{Use: "apply", Short: "Apply an unapplied saved plan, or show a new plan's diff and apply it.", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, arguments []string) error {
			return executeApplicationApply(command, options)
		},
	}
	addDeploymentFlags(command, &options)
	command.Flags().StringVar(&options.planID, "plan", "", "Apply this exact server-side plan ID without reading an apps file.")
	command.Flags().BoolVar(&options.wait, "wait", false, "Wait for rollout completion and fail if the rollout fails.")
	command.Flags().DurationVar(&options.timeout, "timeout", 10*time.Minute, "Maximum local wait time; server execution continues after timeout.")
	return command
}

// These pointers are required for Cobra to populate command-local flag values.
func addApplicationServerFlag(command *cobra.Command, scope *applicationScope) {
	command.Flags().StringVar(&scope.apiUrl, "api-url", "", "Borq API base URL; defaults to the saved server selection.")
	command.Flags().StringVar(&scope.server, "server", "", "Alias for --api-url.")
	_ = command.Flags().MarkHidden("server")
	command.PreRunE = func(command *cobra.Command, arguments []string) error {
		return prepareApplicationScope(command, scope)
	}
}

func addDeploymentFlags(command *cobra.Command, options *deploymentOptions) {
	addApplicationServerFlag(command, &options.scope)
	command.Flags().StringVar(&options.scope.organization, "organization", "", "Organization owning the space.")
	command.Flags().StringVar(&options.scope.space, "space", "", "Deployment space.")
	command.Flags().StringVarP(&options.file, "file", "f", "apps.yaml", "Space configuration with apps, customAddons and managedAddons.")
	command.Flags().StringVar(&options.planFile, "plan-file", "", "Saved plan metadata path; defaults to <file>.borq-plan.json.")
}

func validateApplicationScope(scope applicationScope) error {
	if scope.organization == "" || scope.space == "" {
		return errors.New("provide --organization and --space")
	}
	return nil
}

func executeApplicationPlan(command *cobra.Command, options deploymentOptions) error {
	if err := validateApplicationScope(options.scope); err != nil {
		return err
	}
	client, err := applicationClient(options.scope.server)
	if err != nil {
		return err
	}
	plan, err := createApplicationPlan(command.Context(), client, options, false)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(command.ErrOrStderr(), "Plan %s saved to %s\n", plan.ID, applicationPlanPath(options))
	if err := printApplicationPlanDiff(command.ErrOrStderr(), plan); err != nil {
		return err
	}
	if options.manifests {
		return printApplicationManifests(command.OutOrStdout(), plan.Manifests)
	}
	return printApplicationValue(command.OutOrStdout(), plan.Configuration)
}

func createApplicationPlan(ctx context.Context, client *appapi.ClientWithResponses, options deploymentOptions, automatic bool) (appapi.Plan, error) {
	source, err := os.ReadFile(options.file)
	if err != nil {
		return appapi.Plan{}, fmt.Errorf("read apps file: %w", err)
	}
	patch, err := decodeApplicationPatch(source)
	if err != nil {
		return appapi.Plan{}, err
	}
	response, err := client.PostPlanWithResponse(ctx, options.scope.organization, options.scope.space, patch)
	if err != nil {
		return appapi.Plan{}, err
	}
	if response.JSON201 == nil {
		return appapi.Plan{}, applicationFailure(response.StatusCode(), response.Body)
	}
	metadata := applicationPlanMetadata(options, source, response.JSON201.ID)
	metadata.Automatic = automatic
	if err := saveApplicationPlan(applicationPlanPath(options), metadata); err != nil {
		return appapi.Plan{}, err
	}
	return *response.JSON201, nil
}

func decodeApplicationPatch(source []byte) (appapi.Patch, error) {
	decoder := yamlreader.NewDecoder(bytes.NewReader(source))
	var document any
	if err := decoder.Decode(&document); err != nil {
		return appapi.Patch{}, fmt.Errorf("decode apps file: %w", err)
	}
	if err := decoder.Decode(&document); err != io.EOF {
		return appapi.Patch{}, errors.New("apps file must contain exactly one YAML document")
	}
	// Validate before decoding the generated request model so nulls cannot become
	// omitted fields and explicit zero values and empty lists retain their meaning.
	jsonSource, err := yaml.YAMLToJSONStrict(source)
	if err != nil {
		return appapi.Patch{}, fmt.Errorf("decode apps file: %w", err)
	}
	var value any
	if err := json.Unmarshal(jsonSource, &value); err != nil {
		return appapi.Patch{}, err
	}
	specification, err := appapi.GetSwagger()
	if err != nil {
		return appapi.Patch{}, err
	}
	if err := specification.Components.Schemas["Patch"].Value.VisitJSON(value); err != nil {
		return appapi.Patch{}, errors.New("apps file does not match the OpenAPI configuration schema")
	}
	var patch appapi.Patch
	if err := json.Unmarshal(jsonSource, &patch); err != nil {
		return appapi.Patch{}, fmt.Errorf("decode application request: %w", err)
	}
	return patch, nil
}

func executeApplicationApply(command *cobra.Command, options deploymentOptions) error {
	if options.wait && options.timeout <= 0 {
		return errors.New("--timeout must be greater than zero")
	}
	client, err := applicationClient(options.scope.server)
	if err != nil {
		return err
	}
	planID := options.planID
	if planID == "" {
		if err := validateApplicationScope(options.scope); err != nil {
			return err
		}
		source, readError := os.ReadFile(options.file)
		if readError != nil {
			return fmt.Errorf("read apps file: %w", readError)
		}
		var keys map[string]json.RawMessage
		jsonSource, jsonError := yaml.YAMLToJSONStrict(source)
		if jsonError == nil {
			_ = json.Unmarshal(jsonSource, &keys)
		}
		_, custom := keys["customAddons"]
		_, managed := keys["managedAddons"]
		_, registries := keys["registries"]
		emptyApps := bytes.Equal(bytes.TrimSpace(keys["apps"]), []byte("[]"))
		if custom || managed || registries || emptyApps {
			var fragment appapi.SpaceConfiguration
			if err := decodeManagedDocument(source, "SpaceConfiguration", &fragment); err != nil {
				return err
			}
			response, err := client.ApplySpaceConfiguration(command.Context(), options.scope.organization, options.scope.space, fragment)
			operation, err := readManagedResponse[appapi.Operation](response, err)
			if err != nil {
				return err
			}
			return finishManagedOperation(command, client, operation, options.wait, options.timeout, true)
		}
		planID, err = loadApplicationPlan(command.Context(), client, options)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if planID == "" {
			plan, planError := createApplicationPlan(command.Context(), client, options, true)
			if planError != nil {
				return planError
			}
			if err := printApplicationPlanDiff(command.ErrOrStderr(), plan); err != nil {
				return err
			}
			planID = plan.ID
		}
	}
	response, err := client.ApplyPlanWithResponse(command.Context(), planID)
	if err != nil {
		return err
	}
	if response.JSON202 == nil {
		return applicationFailure(response.StatusCode(), response.Body)
	}
	operation := *response.JSON202
	if options.wait {
		ctx, cancel := context.WithTimeout(command.Context(), options.timeout)
		defer cancel()
		operation, err = waitForApplicationOperation(ctx, client, operation.ID, command.ErrOrStderr())
		if err != nil {
			return err
		}
	}
	return printApplicationValue(command.OutOrStdout(), operation)
}

func waitForApplicationOperation(ctx context.Context, client *appapi.ClientWithResponses, operationID string, progress io.Writer) (appapi.Operation, error) {
	started := time.Now()
	lastMessage := ""
	lastReport := time.Time{}
	for {
		response, err := client.GetOperationWithResponse(ctx, operationID)
		if err != nil {
			return appapi.Operation{}, err
		}
		if response.JSON200 == nil {
			return appapi.Operation{}, applicationFailure(response.StatusCode(), response.Body)
		}
		operation := *response.JSON200
		message := fmt.Sprintf("Rollout %s (%s): %s", operation.Status, operation.Phase, operation.Message)
		for _, resource := range operation.Resources {
			if resource.State != "succeeded" {
				message += fmt.Sprintf("\n  %s %s: %s", resource.Kind, resource.Name, resource.Message)
			}
		}
		if message != lastMessage || time.Since(lastReport) >= 10*time.Second {
			if _, err := fmt.Fprintf(progress, "%s [%s]\n", message, time.Since(started).Round(time.Second)); err != nil {
				return operation, err
			}
			lastMessage, lastReport = message, time.Now()
		}
		switch operation.Status {
		case appapi.Succeeded:
			return operation, nil
		case appapi.Failed, appapi.Canceled, appapi.Superseded:
			return operation, fmt.Errorf("operation %s %s: %s", operation.ID, operation.Status, operation.Message)
		}
		select {
		case <-ctx.Done():
			return operation, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func applicationPlanPath(options deploymentOptions) string {
	if options.planFile != "" {
		return options.planFile
	}
	return options.file + ".borq-plan.json"
}

func applicationPlanMetadata(options deploymentOptions, source []byte, planID string) savedApplicationPlan {
	hash := sha256.Sum256(source)
	return savedApplicationPlan{
		Server: strings.TrimRight(options.scope.server, "/"), Organization: options.scope.organization,
		Space:      options.scope.space,
		SourceHash: hex.EncodeToString(hash[:]), PlanID: planID,
	}
}

func saveApplicationPlan(path string, metadata savedApplicationPlan) error {
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".borq-plan-*")
	if err != nil {
		return fmt.Errorf("save plan metadata: %w", err)
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(append(data, '\n')); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return fmt.Errorf("save plan metadata: %w", err)
	}
	return nil
}

func loadApplicationPlan(ctx context.Context, client *appapi.ClientWithResponses, options deploymentOptions) (string, error) {
	data, err := os.ReadFile(applicationPlanPath(options))
	if err != nil {
		return "", err
	}
	var metadata savedApplicationPlan
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&metadata); err != nil {
		return "", fmt.Errorf("invalid saved plan metadata; run plan again: %w", err)
	}
	if err := decoder.Decode(&metadata); err != io.EOF {
		return "", errors.New("invalid saved plan metadata; run plan again")
	}
	if metadata.PlanID == "" {
		return "", errors.New("invalid saved plan metadata; run plan again")
	}
	source, err := os.ReadFile(options.file)
	if err != nil {
		return "", fmt.Errorf("read apps file for saved plan: %w", err)
	}
	expected := applicationPlanMetadata(options, source, metadata.PlanID)
	expected.Automatic = metadata.Automatic
	if metadata != expected {
		if options.planFile == "" {
			return "", nil
		}
		return "", errors.New("apps file, server, organization, or space changed since planning; run plan again")
	}
	if options.planFile != "" {
		return metadata.PlanID, nil
	}
	response, err := client.GetOperationWithResponse(ctx, metadata.PlanID)
	if err != nil {
		return "", err
	}
	if response.JSON200 != nil {
		// Accepted plans remain available by ID. A normal apply starts fresh after
		// completion, while an unchanged retry can follow its existing rollout.
		switch response.JSON200.Status {
		case appapi.Pending, appapi.Queued, appapi.Applying, appapi.Waiting, appapi.Canceling:
			return metadata.PlanID, nil
		case appapi.Succeeded, appapi.Failed, appapi.Canceled, appapi.Superseded:
		default:
			return "", errors.New("saved plan returned an invalid operation status")
		}
		return "", nil
	}
	if response.StatusCode() != 404 {
		return "", applicationFailure(response.StatusCode(), response.Body)
	}
	if metadata.Automatic {
		return "", nil
	}
	return metadata.PlanID, nil
}

func printApplicationPlanDiff(writer io.Writer, plan appapi.Plan) error {
	if _, err := fmt.Fprintf(writer, "Plan %s\n", plan.ID); err != nil {
		return err
	}
	if plan.ConfigurationDiff == "" {
		if _, err := fmt.Fprintln(writer, "No application configuration changes."); err != nil {
			return err
		}
	} else if _, err := fmt.Fprintln(writer, strings.TrimRight(plan.ConfigurationDiff, "\n")); err != nil {
		return err
	}
	for _, resource := range plan.ResourceDiffs {
		if resource.Action == appapi.Unchanged {
			continue
		}
		name := resource.Name
		if resource.Namespace != "" {
			name = resource.Namespace + "/" + name
		}
		if _, err := fmt.Fprintf(writer, "%s %s %s\n", resource.Action, resource.Kind, name); err != nil {
			return err
		}
	}
	return nil
}

func applicationFailure(status int, body []byte) error {
	var problem appapi.Problem
	if json.Unmarshal(body, &problem) == nil && problem.Message != "" {
		return fmt.Errorf("application API returned %d: %s", status, problem.Message)
	}
	return fmt.Errorf("application API returned HTTP %d", status)
}

func printApplicationValue(writer io.Writer, value any) error {
	data, err := yaml.Marshal(value)
	if err != nil {
		return err
	}
	_, err = writer.Write(data)
	return err
}

func printApplicationManifests(writer io.Writer, manifests []appapi.Manifest) error {
	for index, manifest := range manifests {
		if index > 0 {
			if _, err := fmt.Fprintln(writer, "---"); err != nil {
				return err
			}
		}
		if err := printApplicationValue(writer, manifest); err != nil {
			return err
		}
	}
	return nil
}
