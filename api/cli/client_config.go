package cli

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/stefanmcshane/borq/business/cli/localconfig"
	"sigs.k8s.io/yaml"
)

type clientSettings struct {
	Server       string `json:"server"`
	Organization string `json:"organization"`
	Space        string `json:"space"`
	// Cluster selects the local or registered cluster used by cluster-aware commands.
	Cluster string `json:"cluster,omitempty"`
	// CurrentProfile selects a named override preset without changing base defaults.
	CurrentProfile string `json:"currentProfile,omitempty"`
}

func loadClientSettings() (clientSettings, string, error) {
	directory, err := localconfig.Directory()
	if err != nil {
		return clientSettings{}, "", err
	}
	path := filepath.Join(directory, "config.yaml")
	settings := clientSettings{Server: "http://127.0.0.1:7833", Space: "default"}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := saveClientSettings(path, settings, true); err != nil {
			return clientSettings{}, path, err
		}
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return clientSettings{}, path, fmt.Errorf("read Borq config %s: %w", path, err)
	}
	if err := yaml.UnmarshalStrict(data, &settings); err != nil {
		return clientSettings{}, path, fmt.Errorf("decode Borq config %s: %w", path, err)
	}
	if err := validateClientSettings(settings); err != nil {
		return clientSettings{}, path, fmt.Errorf("invalid Borq config %s: %w", path, err)
	}
	return settings, path, nil
}

func validateClientSettings(settings clientSettings) error {
	server, err := url.Parse(settings.Server)
	if err != nil || (server.Scheme != "http" && server.Scheme != "https") || server.Host == "" {
		return errors.New("server must be an absolute http or https URL")
	}
	if settings.CurrentProfile != "" {
		return validateClientProfileName(settings.CurrentProfile)
	}
	return nil
}

func saveClientSettings(path string, settings clientSettings, createOnly bool) error {
	if err := validateClientSettings(settings); err != nil {
		return err
	}
	data, err := yaml.Marshal(settings)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create Borq config directory: %w", err)
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".config-*")
	if err != nil {
		return fmt.Errorf("write Borq config: %w", err)
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("write Borq config: %w", err)
	}
	if err := file.Close(); err != nil {
		return err
	}
	if createOnly {
		// Publish a complete initial file without replacing settings created by
		// another CLI process while this one was starting.
		if err := os.Link(file.Name(), path); err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("create Borq config: %w", err)
		}
		return nil
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return fmt.Errorf("save Borq config: %w", err)
	}
	return nil
}

// The pointer updates the command-local values captured by its execution closure.
func prepareApplicationScope(command *cobra.Command, scope *applicationScope) error {
	if err := validateApiUrlAliases(command, scope.apiUrl, scope.server); err != nil {
		return err
	}
	settings, err := selectedClientSettings(command)
	if err != nil {
		return err
	}
	if command.Flags().Changed("api-url") {
		scope.server = scope.apiUrl
	} else if !command.Flags().Changed("server") {
		scope.server = settings.Server
	}
	if !command.Flags().Changed("organization") {
		scope.organization = settings.Organization
	}
	if !command.Flags().Changed("space") {
		scope.space = settings.Space
	}
	if !command.Flags().Changed("cluster") {
		scope.cluster = settings.Cluster
	}
	if plan := command.Flags().Lookup("plan"); plan != nil && plan.Value.String() != "" {
		return nil
	}
	if command.Flags().Lookup("organization") != nil && scope.organization == "" {
		return errors.New("provide --organization or set a default with borq config set --organization NAME")
	}
	if command.Flags().Lookup("space") != nil && scope.space == "" {
		return errors.New("provide --space or set a default with borq config set --space NAME")
	}
	return nil
}

// Cobra keeps alias values separate so conflicting flags cannot silently pick
// whichever URL happened to appear last on the command line.
func validateApiUrlAliases(command *cobra.Command, primary, legacy string) error {
	if command.Flags().Changed("api-url") && command.Flags().Changed("server") && strings.TrimRight(primary, "/") != strings.TrimRight(legacy, "/") {
		return errors.New("--api-url and --server specify different API URLs; provide only one")
	}
	return nil
}
