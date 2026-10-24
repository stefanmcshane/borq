package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

func newClientConfigCommand() *cobra.Command {
	group := &cobra.Command{Use: "config", Short: "Manage local defaults and named API, organization, space, and cluster presets."}
	show := &cobra.Command{Use: "show", Short: "Show the resolved base or selected profile settings.", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, arguments []string) error {
			settings, err := selectedClientSettings(command)
			if err != nil {
				return err
			}
			name := displayedProfileName(settings)
			settings.CurrentProfile = ""
			return printApplicationValue(command.OutOrStdout(), map[string]clientSettings{name: settings})
		},
	}
	settings := clientSettings{}
	apiUrl := ""
	set := &cobra.Command{Use: "set", Short: "Edit selected defaults or create a named profile with --profile NAME.", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, arguments []string) error {
			return executeClientConfigSet(command, settings, apiUrl)
		},
	}
	set.Flags().StringVar(&apiUrl, "api-url", "", "Default Borq API base URL.")
	set.Flags().StringVar(&settings.Server, "server", "", "Alias for --api-url.")
	_ = set.Flags().MarkHidden("server")
	set.Flags().StringVar(&settings.Organization, "organization", "", "Selected organization; changing it resets inherited space and cluster selections.")
	set.Flags().StringVar(&settings.Space, "space", "", "Selected space; an empty value requires an explicit deployment flag.")
	set.Flags().StringVar(&settings.Cluster, "cluster", "", "Selected cluster; an empty value clears the selection.")
	use := &cobra.Command{Use: "use NAME", Short: "Select a profile; default selects base settings.", Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			return executeClientConfigUse(command, arguments[0])
		},
	}
	list := &cobra.Command{Use: "list", Short: "List profiles, including the default profile.", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, arguments []string) error {
			_, path, err := loadClientSettings()
			if err != nil {
				return err
			}
			profiles, err := listClientProfiles(path)
			if err != nil {
				return err
			}
			return printApplicationValue(command.OutOrStdout(), profiles)
		},
	}
	current := &cobra.Command{Use: "current", Short: "Print the current profile name.", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, arguments []string) error {
			settings, err := selectedClientSettings(command)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(command.OutOrStdout(), displayedProfileName(settings))
			return err
		},
	}
	group.AddCommand(show, set, use, list, current)
	return group
}

func displayedProfileName(settings clientSettings) string {
	if settings.CurrentProfile == "" {
		return "default"
	}
	return settings.CurrentProfile
}

func executeClientConfigSet(command *cobra.Command, input clientSettings, apiUrl string) error {
	if err := validateApiUrlAliases(command, apiUrl, input.Server); err != nil {
		return err
	}
	updates := command.Flags().Changed("api-url") || command.Flags().Changed("server") || command.Flags().Changed("organization") || command.Flags().Changed("space") || command.Flags().Changed("cluster")
	requested, explicitProfile := changedClientFlag(command, "profile")
	if !updates && (!explicitProfile || requested == "") {
		return errors.New("provide --api-url, --organization, --space, --cluster, or --profile NAME to update defaults")
	}
	settings, path, err := loadClientSettings()
	if err != nil {
		return err
	}
	name, err := requestedClientProfile(command, settings, path)
	if err != nil {
		return err
	}
	if name != "" {
		return executeClientProfileSet(command, settings, path, name, input, apiUrl, explicitProfile)
	}
	if command.Flags().Changed("api-url") {
		settings.Server = apiUrl
	} else if command.Flags().Changed("server") {
		settings.Server = input.Server
	}
	if command.Flags().Changed("organization") {
		if settings.Organization != input.Organization {
			if !command.Flags().Changed("space") {
				settings.Space = "default"
			}
			if !command.Flags().Changed("cluster") {
				settings.Cluster = ""
			}
		}
		settings.Organization = input.Organization
	}
	if command.Flags().Changed("space") {
		settings.Space = input.Space
	}
	if command.Flags().Changed("cluster") {
		settings.Cluster = input.Cluster
	}
	if err := saveClientSettings(path, settings, false); err != nil {
		return err
	}
	settings.CurrentProfile = ""
	return printApplicationValue(command.OutOrStdout(), settings)
}

// Cobra requires the command pointer to distinguish omitted fields from explicit empty overrides.
func executeClientProfileSet(command *cobra.Command, base clientSettings, path, name string, input clientSettings, apiUrl string, allowCreate bool) error {
	profile, err := readClientProfile(path, name)
	if err != nil && (!allowCreate || !errors.Is(err, os.ErrNotExist)) {
		return err
	}
	previous := resolveClientProfile(base, profile, name)
	if command.Flags().Changed("api-url") {
		profile.Server = &apiUrl
	} else if command.Flags().Changed("server") {
		profile.Server = &input.Server
	}
	if command.Flags().Changed("organization") {
		if previous.Organization != input.Organization {
			if !command.Flags().Changed("space") {
				defaultSpace := "default"
				profile.Space = &defaultSpace
			}
			if !command.Flags().Changed("cluster") {
				emptyCluster := ""
				profile.Cluster = &emptyCluster
			}
		}
		profile.Organization = &input.Organization
	}
	if command.Flags().Changed("space") {
		profile.Space = &input.Space
	}
	if command.Flags().Changed("cluster") {
		profile.Cluster = &input.Cluster
	}
	if err := saveClientProfile(path, name, profile, base); err != nil {
		return err
	}
	return printApplicationValue(command.OutOrStdout(), resolveClientProfile(base, profile, name))
}

func executeClientConfigUse(command *cobra.Command, name string) error {
	base, path, err := loadClientSettings()
	if err != nil {
		return err
	}
	if name == "default" {
		if _, err := os.Stat(filepath.Join(filepath.Dir(path), "profiles", name+".yaml")); errors.Is(err, os.ErrNotExist) {
			name = ""
		}
	}
	selected := base
	if name != "" {
		profile, err := readClientProfile(path, name)
		if err != nil {
			return err
		}
		selected = resolveClientProfile(base, profile, name)
		if err := validateClientSettings(selected); err != nil {
			return err
		}
	}
	base.CurrentProfile = name
	if err := saveClientSettings(path, base, false); err != nil {
		return err
	}
	selected.CurrentProfile = name
	return printApplicationValue(command.OutOrStdout(), selected)
}
