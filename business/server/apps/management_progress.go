package apps

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/stefanmcshane/borq/business/server/platform"
	"github.com/stefanmcshane/borq/foundations/kubeapps"
)

// ManagementSetupMessage describes the current setup stage and outstanding
// management resources for installation-administrator diagnostics.
func (manager Manager) ManagementSetupMessage(ctx context.Context) string {
	message := "configuring management services"
	if stored, _, err := loadDocument[componentRecord](ctx, manager.database, "components", "management"); err == nil && stored.Installation.Message != "" {
		message = stored.Installation.Message
	}
	for _, resource := range manager.managementSetupDiagnostics(ctx) {
		message += "; " + strings.Join([]string{resource.Kind, resource.Namespace, resource.Name}, "/") + ": " + resource.Message
	}
	return message
}

func (manager Manager) managementSetupDiagnostics(ctx context.Context) []kubeapps.ResourceStatus {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	resources, err := platform.ReadManagementDiagnostics(ctx, platform.ManagementInput{KubeconfigPath: manager.runtime.managementKubeconfigPath, Settings: manager.runtime.platformSettings})
	if err != nil {
		resources = append(resources, kubeapps.ResourceStatus{Kind: "Management", Name: "diagnostics", State: "running", Message: "some Kubernetes setup diagnostics are unavailable; check cluster access"})
	}
	return resources
}

func (manager Manager) logManagementSetupProgress(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			stored, _, err := loadDocument[componentRecord](ctx, manager.database, "components", "management")
			if err != nil {
				continue
			}
			resources := manager.managementSetupDiagnostics(ctx)
			if ctx.Err() == nil {
				slog.InfoContext(ctx, "management_setup_waiting", "namespace", manager.runtime.platformSettings.Namespace, "message", stored.Installation.Message, "resources", resources)
			}
		}
	}
}
