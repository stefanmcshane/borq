package platform

import (
	"bytes"
	"embed"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"time"

	"sigs.k8s.io/yaml"
)

// Component values and maintenance scripts are part of Borq's versioned catalog,
// rather than external configuration that can drift between installations.
//
//go:embed values/*.yaml values/*.py
var componentValues embed.FS

type component struct {
	Name string
	// Description explains the component's role in deployment infrastructure.
	Description  string
	Chart        string
	Repository   string
	Version      string
	Values       string
	InstallOrder int
	UpgradeOrder int
	// DependsOn must finish chart readiness and credential setup before this component starts.
	DependsOn []string
}

type valuesInput struct {
	Settings
	ClusterID string
}

func normalizeSettings(settings Settings) (Settings, error) {
	settings, err := managedEndpointDefaults(settings)
	if err != nil {
		return Settings{}, err
	}
	settings.Profile = strings.TrimSpace(settings.Profile)
	if settings.Profile == "" {
		settings.Profile = "dev"
	}
	if settings.Profile == "prod" {
		settings.Profile = "production"
	}
	if settings.Profile != "dev" && settings.Profile != "production" {
		return Settings{}, fmt.Errorf("unsupported platform profile %q", settings.Profile)
	}
	if settings.Namespace == "" {
		settings.Namespace = "borq"
	}
	if settings.StorageClass == "" {
		settings.StorageClass = "local-path"
	}
	if settings.Timeout <= 0 {
		settings.Timeout = 5 * time.Minute
	}
	for _, endpoint := range []string{settings.OpenBaoURL, settings.TelemetryURL, settings.LokiURL, settings.PrometheusURL} {
		if endpoint == "" {
			continue
		}
		parsed, err := url.Parse(endpoint)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil {
			return Settings{}, fmt.Errorf("platform endpoint must be an http(s) URL without credentials")
		}
	}
	return settings, nil
}

func managementCatalog(settings Settings, upgrade bool) []component {
	components := []component{
		{Name: "openbao", Chart: "openbao", Repository: "https://openbao.github.io/openbao-helm", Version: "0.30.3", Values: "openbao-" + settings.Profile + ".yaml", InstallOrder: 10, UpgradeOrder: 20},
		{Name: "external-secrets", Chart: "external-secrets", Repository: "https://charts.external-secrets.io", Version: "2.5.0", Values: "external-secrets.yaml", InstallOrder: 20, UpgradeOrder: 10},
		{Name: "prometheus", Chart: "prometheus", Repository: "https://prometheus-community.github.io/helm-charts", Version: "29.36.2", Values: "prometheus.yaml", InstallOrder: 40, UpgradeOrder: 50},
		{Name: "loki", Chart: "loki", Repository: "https://grafana.github.io/helm-charts", Version: "7.2.0", Values: "loki.yaml", InstallOrder: 50, UpgradeOrder: 40},
	}
	if settings.Profile == "dev" {
		// This is the official published archive, served by the upstream GitHub
		// repository as well as charts.rustfs.com, which rejects some networks.
		components = append(components, component{Name: "rustfs", Chart: "https://raw.githubusercontent.com/rustfs/helm/main/rustfs-1.0.1.tgz", Version: "1.0.1", Values: "rustfs.yaml", InstallOrder: 30, UpgradeOrder: 30, DependsOn: []string{"openbao", "external-secrets"}})
		for index := range components {
			if components[index].Name == "loki" {
				components[index].DependsOn = []string{"rustfs"}
			}
		}
	}
	return orderedComponents(components, upgrade)
}

func targetCatalog(upgrade bool) []component {
	return orderedComponents([]component{
		{Name: "cert-manager", Description: "Issues and renews HTTPS certificates.", Chart: "cert-manager", Repository: "https://charts.jetstack.io", Version: "1.21.1", Values: "cert-manager.yaml", InstallOrder: 10, UpgradeOrder: 10},
		{Name: "external-secrets", Description: "Syncs application secrets from the management vault.", Chart: "external-secrets", Repository: "https://charts.external-secrets.io", Version: "2.5.0", Values: "external-secrets.yaml", InstallOrder: 20, UpgradeOrder: 20},
		{Name: "traefik", Description: "Routes web traffic to applications.", Chart: "traefik", Repository: "https://traefik.github.io/charts", Version: "42.0.0", Values: "traefik.yaml", InstallOrder: 30, UpgradeOrder: 40},
		{Name: "alloy", Description: "Collects application metrics and logs.", Chart: "alloy", Repository: "https://grafana.github.io/helm-charts", Version: "1.12.1", Values: "alloy.yaml", InstallOrder: 40, UpgradeOrder: 30, DependsOn: []string{"external-secrets"}},
	}, upgrade)
}

func orderedComponents(components []component, upgrade bool) []component {
	sort.Slice(components, func(left, right int) bool {
		if upgrade {
			return components[left].UpgradeOrder < components[right].UpgradeOrder
		}
		return components[left].InstallOrder < components[right].InstallOrder
	})
	return components
}

func renderComponentValues(definition component, input valuesInput) (map[string]any, error) {
	source, err := componentValues.ReadFile("values/" + definition.Values)
	if err != nil {
		return nil, fmt.Errorf("load %s values: %w", definition.Name, err)
	}
	parsed, err := template.New(definition.Name).Funcs(template.FuncMap{
		"quote":           strconv.Quote,
		"openBaoNodePort": func() int { return OpenBaoNodePort },
		"podHistoryScript": func() (string, error) {
			script, err := componentValues.ReadFile("values/pod-history.py")
			return string(script), err
		},
	}).Parse(string(source))
	if err != nil {
		return nil, fmt.Errorf("parse %s values: %w", definition.Name, err)
	}
	var rendered bytes.Buffer
	if err := parsed.Execute(&rendered, input); err != nil {
		return nil, fmt.Errorf("expand %s values: %w", definition.Name, err)
	}
	var values map[string]any
	if err := yaml.UnmarshalStrict(rendered.Bytes(), &values); err != nil {
		return nil, fmt.Errorf("decode %s values: %w", definition.Name, err)
	}
	return values, nil
}
