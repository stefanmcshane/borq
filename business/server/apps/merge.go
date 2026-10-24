package apps

import (
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"

	"github.com/distribution/reference"
	"k8s.io/apimachinery/pkg/util/validation"
)

// Merge combines a partial update with existing desired state, then validates
// and sorts the result. It never mutates the current state or submitted patch.
func Merge(current Configuration, patch Patch) (Configuration, error) {
	applications := make(map[string]App, len(current.Apps))
	for _, application := range current.Apps {
		if _, exists := applications[application.Name]; exists {
			return Configuration{}, fmt.Errorf("duplicate app %q in stored configuration", application.Name)
		}
		applications[application.Name] = cloneApp(application)
	}
	if patch.Apps != nil {
		if len(*patch.Apps) == 0 {
			applications = map[string]App{}
		}
		seen := make(map[string]bool, len(*patch.Apps))
		for _, update := range *patch.Apps {
			if seen[update.Name] {
				return Configuration{}, fmt.Errorf("duplicate app %q in update", update.Name)
			}
			seen[update.Name] = true
			application, exists := applications[update.Name]
			if !exists {
				application = defaultApp(update.Name, update.Port)
			}
			merged, err := mergeApp(application, update)
			if err != nil {
				return Configuration{}, fmt.Errorf("app %q: %w", update.Name, err)
			}
			applications[update.Name] = merged
		}
	}

	configuration := Configuration{Apps: make([]App, 0, len(applications))}
	for _, application := range applications {
		if application.Env.Version == "" {
			application.Env.Version = "latest"
		}
		var err error
		application.Command, err = normalizeCommand(application.Command)
		if err != nil {
			return Configuration{}, fmt.Errorf("app %q command: %w", application.Name, err)
		}
		application.Predeploy.Command, err = normalizeCommand(application.Predeploy.Command)
		if err != nil {
			return Configuration{}, fmt.Errorf("app %q predeploy command: %w", application.Name, err)
		}
		if err := validateApp(application); err != nil {
			return Configuration{}, fmt.Errorf("app %q: %w", application.Name, err)
		}
		slices.SortFunc(application.Ingresses, func(left, right Ingress) int { return strings.Compare(left.Name, right.Name) })
		configuration.Apps = append(configuration.Apps, application)
	}
	slices.SortFunc(configuration.Apps, func(left, right App) int { return strings.Compare(left.Name, right.Name) })
	if err := validateIngressResourceNames(configuration); err != nil {
		return Configuration{}, err
	}
	return configuration, nil
}

func defaultApp(name string, port *int) App {
	listener := 0
	if port != nil {
		listener = *port
	}
	return App{
		Name: name, Replicas: 1,
		Env: EnvReference{Version: "latest"}, Ingresses: []Ingress{}, Predeploy: Predeploy{TimeoutSeconds: 300},
		Checks: Checks{
			Readiness: Check{Enabled: listener > 0, Path: "/healthz", Port: listener, PeriodSeconds: 10, TimeoutSeconds: 1, FailureThreshold: 3},
			Startup:   Check{Enabled: listener > 0, Path: "/healthz", Port: listener, PeriodSeconds: 10, TimeoutSeconds: 1, FailureThreshold: 3},
		},
	}
}

func cloneApp(application App) App {
	application.Ingresses = append([]Ingress{}, application.Ingresses...)
	return application
}

func mergeApp(application App, update AppPatch) (App, error) {
	if update.Image != nil {
		application.Image = *update.Image
	}
	if update.Port != nil {
		application.Port = *update.Port
	}
	if update.Replicas != nil {
		application.Replicas = *update.Replicas
	}
	if update.Command != nil {
		application.Command = *update.Command
	}
	if update.Env != nil {
		if update.Env.Name != "" {
			application.Env.Name = update.Env.Name
		}
		application.Env.Version = "latest"
		if update.Env.Version != "" {
			application.Env.Version = update.Env.Version
		}
	}
	if update.Ingresses != nil {
		ingresses, err := mergeIngresses(application.Ingresses, *update.Ingresses)
		if err != nil {
			return App{}, err
		}
		application.Ingresses = ingresses
	}
	if update.Predeploy != nil {
		application.Predeploy = mergePredeploy(application.Predeploy, *update.Predeploy)
	}
	if update.Checks != nil {
		if update.Checks.Readiness != nil {
			application.Checks.Readiness = mergeCheck(application.Checks.Readiness, *update.Checks.Readiness)
		}
		if update.Checks.Startup != nil {
			application.Checks.Startup = mergeCheck(application.Checks.Startup, *update.Checks.Startup)
		}
	}
	return application, nil
}

func mergeIngresses(current, updates []Ingress) ([]Ingress, error) {
	ingresses := make(map[string]Ingress, len(current)+len(updates))
	if len(updates) > 0 {
		for _, ingress := range current {
			ingresses[ingress.Name] = ingress
		}
	}
	seen := make(map[string]bool, len(updates))
	for _, ingress := range updates {
		if seen[ingress.Name] {
			return nil, fmt.Errorf("duplicate ingress %q", ingress.Name)
		}
		seen[ingress.Name] = true
		if ingress.Path == "" {
			ingress.Path = "/"
		}
		ingresses[ingress.Name] = ingress
	}
	merged := make([]Ingress, 0, len(ingresses))
	for _, ingress := range ingresses {
		merged = append(merged, ingress)
	}
	return merged, nil
}

func mergeCheck(check Check, update CheckPatch) Check {
	if update.Enabled != nil {
		check.Enabled = *update.Enabled
	}
	if update.Path != nil {
		check.Path = *update.Path
	}
	if update.Port != nil {
		check.Port = *update.Port
	}
	if update.PeriodSeconds != nil {
		check.PeriodSeconds = *update.PeriodSeconds
	}
	if update.TimeoutSeconds != nil {
		check.TimeoutSeconds = *update.TimeoutSeconds
	}
	if update.FailureThreshold != nil {
		check.FailureThreshold = *update.FailureThreshold
	}
	return check
}

func validateApp(application App) error {
	if len(application.Name) > 50 {
		return fmt.Errorf("name must be at most 50 characters")
	}
	if errors := validation.IsDNS1035Label(application.Name); len(errors) > 0 {
		return fmt.Errorf("name must be a Kubernetes Service name: %s", strings.Join(errors, "; "))
	}
	if application.Image == "" {
		return fmt.Errorf("image is required")
	}
	if _, err := reference.ParseNormalizedNamed(application.Image); err != nil {
		return fmt.Errorf("invalid image %q: %w", application.Image, err)
	}
	if application.Port < 0 || application.Port > 65535 {
		return fmt.Errorf("port must be between 0 and 65535")
	}
	if application.Replicas < 0 || int64(application.Replicas) > 2147483647 {
		return fmt.Errorf("replicas must be between 0 and 2147483647")
	}
	if err := validateEnvReference(application.Env); err != nil {
		return err
	}
	if err := validateIngresses(application); err != nil {
		return err
	}
	if err := validatePredeploy(application.Predeploy); err != nil {
		return err
	}
	if err := validateCheck(application.Checks.Readiness); err != nil {
		return fmt.Errorf("readiness: %w", err)
	}
	if err := validateCheck(application.Checks.Startup); err != nil {
		return fmt.Errorf("startup: %w", err)
	}
	return nil
}

func validateIngresses(application App) error {
	if len(application.Ingresses) > 0 && application.Port == 0 {
		return fmt.Errorf("ingresses require a nonzero application port")
	}
	seen := make(map[string]bool, len(application.Ingresses))
	for _, ingress := range application.Ingresses {
		if errors := validation.IsDNS1123Label(ingress.Name); len(errors) > 0 {
			return fmt.Errorf("invalid ingress name %q: %s", ingress.Name, strings.Join(errors, "; "))
		}
		if seen[ingress.Name] {
			return fmt.Errorf("duplicate ingress %q", ingress.Name)
		}
		seen[ingress.Name] = true
		hostErrors := validation.IsDNS1123Subdomain(ingress.Host)
		if strings.HasPrefix(ingress.Host, "*.") {
			hostErrors = validation.IsWildcardDNS1123Subdomain(ingress.Host)
		}
		if len(hostErrors) > 0 || ingress.Host == "" || net.ParseIP(ingress.Host) != nil {
			return fmt.Errorf("invalid ingress host %q", ingress.Host)
		}
		if !validHttpPath(ingress.Path) {
			return fmt.Errorf("ingress %q path must be an absolute HTTP path", ingress.Name)
		}
	}
	return nil
}

func validateCheck(check Check) error {
	if !validHttpPath(check.Path) {
		return fmt.Errorf("path must be an absolute HTTP path")
	}
	if check.Port < 0 || check.Port > 65535 || (check.Enabled && check.Port == 0) {
		return fmt.Errorf("enabled probe port must be between 1 and 65535")
	}
	if check.PeriodSeconds < 1 || check.TimeoutSeconds < 1 || check.FailureThreshold < 1 {
		return fmt.Errorf("periodSeconds, timeoutSeconds and failureThreshold must be positive")
	}
	if int64(check.PeriodSeconds) > 2147483647 || int64(check.TimeoutSeconds) > 2147483647 || int64(check.FailureThreshold) > 2147483647 {
		return fmt.Errorf("probe timing values must fit Kubernetes integer fields")
	}
	return nil
}

func validateIngressResourceNames(configuration Configuration) error {
	owners := make(map[string]string)
	for _, application := range configuration.Apps {
		for _, ingress := range application.Ingresses {
			resourceName := application.Name + "-" + ingress.Name
			if owner, exists := owners[resourceName]; exists {
				return fmt.Errorf("ingress resource %q is shared by %s and %s/%s", resourceName, owner, application.Name, ingress.Name)
			}
			owners[resourceName] = application.Name + "/" + ingress.Name
		}
	}
	return nil
}

func validHttpPath(path string) bool {
	parsed, err := url.ParseRequestURI(path)
	return err == nil && strings.HasPrefix(path, "/") && !strings.HasPrefix(path, "//") &&
		!strings.ContainsAny(path, " \t\r\n#?") && parsed.Host == "" && parsed.Scheme == ""
}
