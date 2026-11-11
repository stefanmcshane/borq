package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/spf13/cobra"
	"github.com/stefanmcshane/borq/api/appapi"
	"github.com/stefanmcshane/borq/business/offsite"
)

func newManagementApplyCommand() *cobra.Command {
	scope := applicationScope{}
	file := "management.yaml"
	recoveryFile := ""
	tokenFile := ""
	wait := true
	timeout := 15 * time.Minute
	command := &cobra.Command{Use: "apply", Short: "Apply management configuration, including component upgrades.", Args: cobra.NoArgs, RunE: func(command *cobra.Command, arguments []string) error {
		return executeManagementApply(command, scope, file, recoveryFile, tokenFile, wait, timeout)
	}}
	addApplicationServerFlag(command, &scope)
	command.Flags().StringVarP(&file, "file", "f", file, "Unified management YAML document.")
	command.Flags().StringVar(&tokenFile, "token-file", "", "Administrator token for this command only; does not save a login.")
	command.Flags().StringVar(&recoveryFile, "recovery-config", "", "Private independently retained offsite recovery JSON file; supplies transient repository credentials.")
	command.Flags().BoolVar(&wait, "wait", wait, "Wait for aggregate apply to complete.")
	command.Flags().DurationVar(&timeout, "timeout", timeout, "Maximum local wait; server work continues after timeout.")
	command.Flags().String("output", "cli", "Output format: cli or json.")
	return command
}

func executeManagementApply(command *cobra.Command, scope applicationScope, file, recoveryFile, tokenFile string, wait bool, timeout time.Duration) error {
	output, _ := command.Flags().GetString("output")
	if output != "cli" && output != "json" {
		return errors.New("--output must be cli or json")
	}
	if command.Flags().Changed("token-file") && tokenFile == "" {
		return errors.New("--token-file must name a private administrator token file")
	}
	if wait && timeout <= 0 {
		return errors.New("--timeout must be greater than zero")
	}
	body, err := readManagementConfiguration(file, recoveryFile)
	if err != nil {
		return err
	}
	client, err := applicationClientForCommand(scope.server, tokenFile)
	if err != nil {
		return err
	}
	return applyManagementConfigurationWithClient(command, client, body, wait, timeout)
}

func readManagementConfiguration(file, recoveryFile string) (appapi.ManagementConfiguration, error) {
	var body appapi.ManagementConfiguration
	if err := readManagedDocument(file, "ManagementConfiguration", &body); err != nil {
		return body, err
	}
	if recoveryFile != "" {
		var recovery offsite.RecoveryConfig
		if err := offsite.ReadPrivateConfig(recoveryFile, &recovery); err != nil {
			return body, err
		}
		if body.Offsite == nil {
			return body, errors.New("--recovery-config requires an offsite policy in management configuration")
		}
		policy := body.Offsite
		if policy.CredentialsRef != nil || policy.Password != nil || policy.AccessKey != nil || policy.SecretKey != nil || policy.SessionToken != nil {
			return body, errors.New("supply offsite credentials through one recovery configuration")
		}
		repository := recovery.Repository
		if err := repository.Validate(); err != nil {
			return body, err
		}
		if policy.Repository != nil && *policy.Repository != repository.URL {
			return body, errors.New("offsite repository differs from recovery configuration")
		}
		if policy.Region != nil && *policy.Region != repository.Region {
			return body, errors.New("offsite region differs from recovery configuration")
		}
		policy.Repository, policy.Region = &repository.URL, &repository.Region
		policy.Password, policy.AccessKey, policy.SecretKey = &repository.Password, &repository.AccessKey, &repository.SecretKey
		if repository.SessionToken != "" {
			policy.SessionToken = &repository.SessionToken
		}
	}
	return body, nil
}

// The generated API client is shared by submission and operation polling.
func applyManagementConfigurationWithClient(command *cobra.Command, client *appapi.ClientWithResponses, body appapi.ManagementConfiguration, wait bool, timeout time.Duration) error {
	ctx := command.Context()
	started := time.Now()
	if wait {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	response, err := client.ApplyManagementConfiguration(ctx, body)
	lastMessage := ""
	lastPrinted := time.Time{}
	for wait && err == nil && response.StatusCode == http.StatusServiceUnavailable && response.Header.Get("X-Borq-Setup-Pending") == "true" {
		// Startup rejects this request before it queues any configuration changes.
		var problem appapi.Problem
		_ = json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&problem)
		_ = response.Body.Close()
		message := "Waiting for Borq's supporting services before applying configuration: " + problem.Message
		if message != lastMessage || time.Since(lastPrinted) >= 10*time.Second {
			if _, err := fmt.Fprintf(command.ErrOrStderr(), "%s [elapsed %s]\n", message, time.Since(started).Round(time.Second)); err != nil {
				return err
			}
			lastMessage, lastPrinted = message, time.Now()
		}
		seconds, _ := strconv.Atoi(response.Header.Get("Retry-After"))
		timer := time.NewTimer(time.Duration(max(1, min(30, seconds))) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("management services are not ready; server reconciliation continues, rerun management apply to retry: %w; last observed: %s", ctx.Err(), lastMessage)
		case <-timer.C:
		}
		response, err = client.ApplyManagementConfiguration(ctx, body)
	}
	operation, err := readManagedResponse[appapi.Operation](response, err)
	if err != nil {
		if lastMessage != "" {
			return fmt.Errorf("management setup request failed: %w; last observed: %s", err, lastMessage)
		}
		return err
	}
	if err := printManagementOidcCallbacks(ctx, command, client, body); err != nil {
		_, _ = fmt.Fprintln(command.ErrOrStderr(), "OIDC callback URLs could not be read. Use borq org oidc callback NAME --issuer-url ISSUER --borq-public-url URL to retrieve them independently of this apply.")
	}
	if wait {
		timeout -= time.Since(started)
		operation, err = waitManagedOperation(command, client, operation, timeout, true)
		if err != nil {
			return err
		}
	}
	output, _ := command.Flags().GetString("output")
	if output == "json" {
		return json.NewEncoder(command.OutOrStdout()).Encode(operation)
	}
	if !wait {
		return printApplicationValue(command.OutOrStdout(), operation)
	}
	if _, err := fmt.Fprintln(command.OutOrStdout(), "Management configuration applied."); err != nil {
		return err
	}
	return printManagementSetup(command, client)
}

func newManagementExportCommand() *cobra.Command {
	scope := applicationScope{}
	tokenFile := ""
	command := &cobra.Command{Use: "export", Short: "Export management configuration with safe credential references.", Args: cobra.NoArgs, RunE: func(command *cobra.Command, arguments []string) error {
		return executeManagementExport(command, scope, tokenFile)
	}}
	addApplicationServerFlag(command, &scope)
	command.Flags().StringVar(&tokenFile, "token-file", "", "Administrator token for this command only; does not save a login.")
	return command
}

func executeManagementExport(command *cobra.Command, scope applicationScope, tokenFile string) error {
	if command.Flags().Changed("token-file") && tokenFile == "" {
		return errors.New("--token-file must name a private administrator token file")
	}
	client, err := applicationClientForCommand(scope.server, tokenFile)
	if err != nil {
		return err
	}
	response, err := client.GetManagementConfiguration(command.Context())
	value, err := readManagedResponse[appapi.ManagementConfiguration](response, err)
	if err != nil {
		return err
	}
	return printApplicationValue(command.OutOrStdout(), value)
}
