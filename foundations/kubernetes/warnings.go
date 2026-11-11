package kubernetes

import (
	"context"
	"log/slog"
)

// apiWarningLogger preserves Kubernetes warnings without using controller-runtime's
// process-global deferred logger, which emits a stack when it is uninitialized.
type apiWarningLogger struct{}

func (apiWarningLogger) HandleWarningHeaderWithContext(ctx context.Context, code int, agent, message string) {
	if code != 299 || message == "" {
		return
	}
	slog.WarnContext(ctx, "kubernetes_api_warning", "message", message, "code", code, "agent", agent)
}
