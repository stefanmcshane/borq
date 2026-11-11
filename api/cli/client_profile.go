package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"
)

// Pointer fields preserve intentional empty overrides separately from inheritance.
// The preset stays partial so later changes to base defaults remain visible.
type clientProfileSettings struct {
	// Server overrides the API address when supplied.
	Server *string `json:"server,omitempty"`
	// Organization overrides or clears the selected organization.
	Organization *string `json:"organization,omitempty"`
	// Space overrides or clears the selected deployment space.
	Space *string `json:"space,omitempty"`
	// Cluster overrides or clears the selected cluster.
	Cluster *string `json:"cluster,omitempty"`
}

func validateClientProfileName(name string) error {
	if !regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`).MatchString(name) {
		return errors.New("profile name must start with a lowercase letter or digit and contain at most 63 lowercase letters, digits, dots, hyphens, or underscores")
	}
	return nil
}

// The command pointer accesses Cobra's inherited selection without global state.
func requestedClientProfile(command *cobra.Command, base clientSettings, basePath string) (string, error) {
	name := base.CurrentProfile
	if flag := command.Flag("profile"); flag != nil && flag.Changed {
		name = flag.Value.String()
	}
	if name == "default" {
		if _, err := os.Stat(filepath.Join(filepath.Dir(basePath), "profiles", name+".yaml")); errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
	}
	if name != "" {
		if err := validateClientProfileName(name); err != nil {
			return "", err
		}
	}
	return name, nil
}

func clientProfilePath(basePath, name string) (string, error) {
	if err := validateClientProfileName(name); err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(basePath), "profiles", name+".yaml"), nil
}

func readClientProfile(basePath, name string) (clientProfileSettings, error) {
	path, err := clientProfilePath(basePath, name)
	if err != nil {
		return clientProfileSettings{}, err
	}
	source, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return clientProfileSettings{}, fmt.Errorf("profile %q does not exist; create it with borq config set --profile %s: %w", name, name, err)
	}
	if err != nil {
		return clientProfileSettings{}, fmt.Errorf("read profile %s: %w", path, err)
	}
	var profile clientProfileSettings
	if err := yaml.UnmarshalStrict(source, &profile); err != nil {
		return clientProfileSettings{}, fmt.Errorf("decode profile %s: %w", path, err)
	}
	return profile, nil
}

func resolveClientProfile(base clientSettings, profile clientProfileSettings, name string) clientSettings {
	selected := base
	selected.CurrentProfile = name
	if profile.Server != nil {
		selected.Server = *profile.Server
	}
	if profile.Organization != nil {
		if selected.Organization != *profile.Organization {
			selected.Space = "default"
			selected.Cluster = ""
		}
		selected.Organization = *profile.Organization
	}
	if profile.Space != nil {
		selected.Space = *profile.Space
	}
	if profile.Cluster != nil {
		selected.Cluster = *profile.Cluster
	}
	return selected
}

// The command pointer reads the invocation's profile flag through Cobra.
func selectedClientSettings(command *cobra.Command) (clientSettings, error) {
	base, path, err := loadClientSettings()
	if err != nil {
		return clientSettings{}, err
	}
	name, err := requestedClientProfile(command, base, path)
	if err != nil {
		return clientSettings{}, err
	}
	if name == "" {
		base.CurrentProfile = ""
		return applyCommandClientSettings(command, base)
	}
	profile, err := readClientProfile(path, name)
	if err != nil {
		return clientSettings{}, err
	}
	selected := resolveClientProfile(base, profile, name)
	if err := validateClientSettings(selected); err != nil {
		return clientSettings{}, fmt.Errorf("invalid profile %q: %w", name, err)
	}
	return applyCommandClientSettings(command, selected)
}

// The command pointer reads only explicitly supplied common scope flags.
func applyCommandClientSettings(command *cobra.Command, selected clientSettings) (clientSettings, error) {
	primary, primarySet := changedClientFlag(command, "api-url")
	legacy, legacySet := changedClientFlag(command, "server")
	if err := validateApiUrlAliases(command, primary, legacy); err != nil {
		return clientSettings{}, err
	}
	if primarySet {
		selected.Server = primary
	} else if legacySet {
		selected.Server = legacy
	}
	if organization, supplied := changedClientFlag(command, "organization"); supplied {
		if organization != selected.Organization {
			selected.Space = "default"
		}
		selected.Organization = organization
	}
	if space, supplied := changedClientFlag(command, "space"); supplied {
		selected.Space = space
	}
	if cluster, supplied := changedClientFlag(command, "cluster"); supplied {
		selected.Cluster = cluster
	}
	if err := validateClientSettings(selected); err != nil {
		return clientSettings{}, err
	}
	return selected, nil
}

// Cobra owns inherited flag values, which must be accessed through the command pointer.
func changedClientFlag(command *cobra.Command, name string) (string, bool) {
	flag := command.Flag(name)
	if flag == nil || !flag.Changed {
		return "", false
	}
	return flag.Value.String(), true
}

func saveClientProfile(basePath, name string, profile clientProfileSettings, base clientSettings) error {
	path, err := clientProfilePath(basePath, name)
	if err != nil {
		return err
	}
	if err := validateClientSettings(resolveClientProfile(base, profile, name)); err != nil {
		return fmt.Errorf("invalid profile %q: %w", name, err)
	}
	data, err := yaml.Marshal(profile)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create profile directory: %w", err)
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".profile-*")
	if err != nil {
		return fmt.Errorf("create profile: %w", err)
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return fmt.Errorf("save profile: %w", err)
	}
	return nil
}

func listClientProfiles(basePath string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(filepath.Dir(basePath), "profiles"))
	if errors.Is(err, os.ErrNotExist) {
		return []string{"default"}, nil
	}
	if err != nil {
		return nil, err
	}
	profiles := []string{"default"}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".yaml")
		if err := validateClientProfileName(name); err != nil {
			continue
		}
		if name == "default" {
			continue
		}
		profiles = append(profiles, name)
	}
	sort.Strings(profiles)
	return profiles, nil
}

// The command pointer identifies profile creation, which intentionally permits
// a name that is not on disk yet. Other explicit selections must already exist.
func validateCommandProfile(command *cobra.Command) error {
	flag := command.Flag("profile")
	if flag == nil || !flag.Changed || flag.Value.String() == "" {
		return nil
	}
	if command.Name() == "set" && command.Parent() != nil && command.Parent().Name() == "config" {
		return validateClientProfileName(flag.Value.String())
	}
	_, err := selectedClientSettings(command)
	return err
}
