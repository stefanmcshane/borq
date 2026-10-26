package cli

import (
	"errors"
	"github.com/spf13/cobra"
	"github.com/stefanmcshane/borq/api/appapi"
	"github.com/stefanmcshane/borq/business/offsite"
	"time"
)

func newManagementApplyCommand() *cobra.Command {
	scope := applicationScope{}
	file := "management.yaml"
	recoveryFile := ""
	wait := true
	timeout := 15 * time.Minute
	command := &cobra.Command{Use: "apply", Short: "Apply management configuration, including component upgrades.", Args: cobra.NoArgs, RunE: func(command *cobra.Command, arguments []string) error {
		return executeManagementApply(command, scope, file, recoveryFile, wait, timeout)
	}}
	addApplicationServerFlag(command, &scope)
	command.Flags().StringVarP(&file, "file", "f", file, "Unified management YAML document.")
	command.Flags().StringVar(&recoveryFile, "recovery-config", "", "Private independently retained offsite recovery JSON file; supplies transient repository credentials.")
	command.Flags().BoolVar(&wait, "wait", wait, "Wait for aggregate apply to complete.")
	command.Flags().DurationVar(&timeout, "timeout", timeout, "Maximum local wait; server work continues after timeout.")
	return command
}

func executeManagementApply(command *cobra.Command, scope applicationScope, file, recoveryFile string, wait bool, timeout time.Duration) error {
	if wait && timeout <= 0 {
		return errors.New("--timeout must be greater than zero")
	}
	var body appapi.ManagementConfiguration
	if err := readManagedDocument(file, "ManagementConfiguration", &body); err != nil {
		return err
	}
	if recoveryFile != "" {
		var recovery offsite.RecoveryConfig
		if err := offsite.ReadPrivateConfig(recoveryFile, &recovery); err != nil {
			return err
		}
		if body.Offsite == nil {
			return errors.New("--recovery-config requires an offsite policy in management configuration")
		}
		policy := body.Offsite
		if policy.CredentialsRef != nil || policy.Password != nil || policy.AccessKey != nil || policy.SecretKey != nil || policy.SessionToken != nil {
			return errors.New("supply offsite credentials through one recovery configuration")
		}
		repository := recovery.Repository
		if err := repository.Validate(); err != nil {
			return err
		}
		if policy.Repository != nil && *policy.Repository != repository.URL {
			return errors.New("offsite repository differs from recovery configuration")
		}
		if policy.Region != nil && *policy.Region != repository.Region {
			return errors.New("offsite region differs from recovery configuration")
		}
		policy.Repository, policy.Region = &repository.URL, &repository.Region
		policy.Password, policy.AccessKey, policy.SecretKey = &repository.Password, &repository.AccessKey, &repository.SecretKey
		if repository.SessionToken != "" {
			policy.SessionToken = &repository.SessionToken
		}
	}
	client, err := applicationClient(scope.server)
	if err != nil {
		return err
	}
	response, err := client.ApplyManagementConfiguration(command.Context(), body)
	operation, err := readManagedResponse[appapi.Operation](response, err)
	if err != nil {
		return err
	}
	return finishManagedOperation(command, client, operation, wait, timeout, true)
}

func newManagementExportCommand() *cobra.Command {
	scope := applicationScope{}
	command := &cobra.Command{Use: "export", Short: "Export management configuration with safe credential references.", Args: cobra.NoArgs, RunE: func(command *cobra.Command, arguments []string) error {
		return executeManagementExport(command, scope)
	}}
	addApplicationServerFlag(command, &scope)
	return command
}

func executeManagementExport(command *cobra.Command, scope applicationScope) error {
	client, err := applicationClient(scope.server)
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
