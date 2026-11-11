package cli

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"github.com/stefanmcshane/borq/api/appapi"
	"github.com/stefanmcshane/borq/business/cli/management"
)

func readManagementBootstrapConfiguration(file string) (appapi.ManagementConfiguration, error) {
	configuration, err := readManagementConfiguration(file, "")
	if err != nil {
		return configuration, err
	}
	if configuration.Organizations == nil {
		return configuration, nil
	}
	for _, organization := range *configuration.Organizations {
		if organization.ManagedPocketID != nil {
			return configuration, errors.New("deploy Pocket ID separately after bootstrap using management apply or as a managed application on an application cluster; use oidc to connect an existing provider during bootstrap")
		}
		if organization.Spaces == nil {
			continue
		}
		for _, space := range *organization.Spaces {
			if space.ManagedAddons != nil && space.ManagedAddons.Pocketid != nil && len(*space.ManagedAddons.Pocketid) != 0 {
				return configuration, errors.New("deploy Pocket ID as a managed application after bootstrap; use oidc to connect an existing provider during bootstrap")
			}
		}
	}
	return configuration, nil
}

func applyManagementBootstrapConfiguration(command *cobra.Command, result management.Result, configuration appapi.ManagementConfiguration, timeout time.Duration) error {
	client, err := applicationClientWithToken(result.ApiUrl, result.AdminToken)
	if result.KubernetesProxy {
		if result.HttpClient == nil {
			return errors.New("bootstrap completed without its private API connection")
		}
		client, err = appapi.NewClientWithResponses(result.ApiUrl, appapi.WithHTTPClient(result.HttpClient))
	}
	if err != nil {
		return err
	}
	if err := applyManagementConfigurationWithClient(command, client, configuration, true, timeout); err != nil {
		return fmt.Errorf("Borq is installed, but management configuration failed; correct the file and retry with borq management apply: %w", err)
	}
	if output, _ := command.Flags().GetString("output"); output == "json" {
		return nil
	}
	_, err = fmt.Fprintln(command.OutOrStdout(), "The administrator token remains available for setup and recovery.")
	return err
}
