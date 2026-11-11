package platform

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/stefanmcshane/borq/foundations/helm"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"go.opentelemetry.io/otel/trace"
)

type componentRun struct {
	connection        kubernetes.Connection
	kubeconfigPath    string
	scope             string
	values            valuesInput
	definitions       []component
	upgrade           bool
	upgradeComponents []string
	recoverPending    bool
	recoverComponents []string
	target            TargetInput
}

func reconcileComponents(ctx context.Context, input componentRun) (Result, error) {
	queuedAt := time.Now()
	log := slog.With("scope", input.scope, "namespace", input.values.Namespace, "upgrade", input.upgrade)
	if input.scope == "target" {
		log = log.With("organization", input.target.Organization, "cluster", input.target.Name, "cluster_id", input.target.ID)
	}
	if span := trace.SpanContextFromContext(ctx); span.IsValid() {
		log = log.With("trace_id", span.TraceID().String(), "span_id", span.SpanID().String())
	}
	log.InfoContext(ctx, "components_setup_started", "components", len(input.definitions))
	finishRun := func(err error) {
		elapsed := time.Since(queuedAt)
		level, status := slog.LevelInfo, "ready"
		fields := []any{"duration", elapsed.Round(time.Millisecond).String(), "duration_ms", elapsed.Milliseconds()}
		if err != nil {
			level, status = slog.LevelError, "failed"
			fields = append(fields, "error", err)
		}
		log.Log(ctx, level, "components_setup_finished", append(fields, "status", status)...)
	}
	result := Result{Profile: input.values.Profile, Namespace: input.values.Namespace, Components: make([]ComponentState, len(input.definitions))}
	finished := make(map[string]chan struct{}, len(input.definitions))
	positions := make(map[string]int, len(input.definitions))
	failures := make([]error, len(input.definitions))
	for index, definition := range input.definitions {
		result.Components[index] = ComponentState{Name: definition.Name, Namespace: definition.namespace(input.values.Namespace), Chart: definition.Chart, Version: definition.Version, Status: "pending"}
		finished[definition.Name] = make(chan struct{})
		positions[definition.Name] = index
	}
	if err := persistState(ctx, input.connection, input.scope, result); err != nil {
		finishRun(err)
		return result, err
	}
	var stateMutex sync.Mutex
	record := func(index int, state ComponentState) error {
		stateMutex.Lock()
		defer stateMutex.Unlock()
		result.Components[index] = state
		return persistState(ctx, input.connection, input.scope, result)
	}
	// Bound chart downloads, API requests and readiness waits on small clusters.
	slots := make(chan struct{}, 3)
	var workers sync.WaitGroup
	for index, definition := range input.definitions {
		workers.Add(1)
		go func() {
			defer workers.Done()
			// Closing publishes this component's failure before dependants inspect it.
			defer close(finished[definition.Name])
			componentLog := log.With("component", definition.Name, "chart", definition.Chart)
			var startedAt time.Time
			state := ComponentState{Name: definition.Name, Namespace: definition.namespace(input.values.Namespace), Chart: definition.Chart, Version: definition.Version, Status: "pending"}
			finish := func(err error) {
				if err != nil && (state.Status == "pending" || state.Status == "installing") {
					state.Status = "failed"
				}
				failures[index] = errors.Join(err, record(index, state))
				elapsed, waiting := time.Duration(0), time.Since(queuedAt)
				if !startedAt.IsZero() {
					elapsed, waiting = time.Since(startedAt), startedAt.Sub(queuedAt)
				}
				level := slog.LevelInfo
				fields := []any{
					"chart_version", state.Version, "status", state.Status, "changed", state.Changed, "revision", state.Revision,
					"duration", elapsed.Round(time.Millisecond).String(), "duration_ms", elapsed.Milliseconds(),
					"wait_duration_ms", waiting.Milliseconds(),
				}
				if failures[index] != nil {
					level = slog.LevelError
					fields = append(fields, "error", failures[index])
				}
				if state.Status == "blocked" {
					level = slog.LevelWarn
				}
				componentLog.Log(ctx, level, "component_setup_finished", fields...)
			}
			dependencies := slices.Clone(definition.DependsOn)
			if input.upgrade && index > 0 {
				// Explicit upgrades retain their separately declared serial order.
				dependencies = append(dependencies, input.definitions[index-1].Name)
			}
			for _, dependency := range dependencies {
				select {
				case <-finished[dependency]:
				case <-ctx.Done():
					finish(ctx.Err())
					return
				}
				if failures[positions[dependency]] != nil {
					state.Status = "blocked"
					finish(fmt.Errorf("component %s is blocked by failed dependency %s", definition.Name, dependency))
					return
				}
				if input.upgrade && componentSelected(input.upgradeComponents, definition.Name) && slices.Contains(definition.DependsOn, dependency) && result.Components[positions[dependency]].Status != "deployed" {
					state.Status = "blocked"
					finish(fmt.Errorf("component %s requires deployed dependency %s", definition.Name, dependency))
					return
				}
			}
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				finish(ctx.Err())
				return
			}
			startedAt = time.Now()
			componentLog.InfoContext(ctx, "component_setup_started", "chart_version", definition.Version)
			state.Status = "installing"
			if err := record(index, state); err != nil {
				finish(err)
				return
			}
			var err error
			state, err = input.reconcile(ctx, definition)
			finish(err)
		}()
	}
	workers.Wait()
	err := errors.Join(failures...)
	finishRun(err)
	return result, err
}

func (input componentRun) reconcile(ctx context.Context, definition component) (ComponentState, error) {
	state := ComponentState{Name: definition.Name, Namespace: definition.namespace(input.values.Namespace), Chart: definition.Chart, Version: definition.Version, Status: "failed"}
	if input.upgrade && !componentSelected(input.upgradeComponents, definition.Name) {
		result, err := readComponents(ctx, input.connection, input.kubeconfigPath, input.values, []component{definition})
		if err != nil {
			return state, err
		}
		return result.Components[0], nil
	}
	if err := input.prepareNetworkComponent(ctx, definition); err != nil {
		return state, err
	}
	if definition.Name == "tailscale-operator" {
		tag, err := networkOperatorTag(ctx, input.connection, input.values)
		if err != nil {
			return state, err
		}
		input.values.NetworkOperatorTag = tag
	}
	if definition.Name == "external-dns" {
		owner, err := networkOwnerID(ctx, input.connection)
		if err != nil {
			return state, err
		}
		input.values.NetworkOwner = owner
	}
	if definition.Name == "rustfs" {
		if err := prepareRustfs(ctx, input.connection, input.values.Settings); err != nil {
			return state, fmt.Errorf("prepare rustfs credentials: %w", err)
		}
	}
	if definition.Name == "loki" {
		if err := prepareSharedS3(ctx, input.connection, input.values.Settings); err != nil {
			return state, err
		}
	}
	if definition.Name == "alloy" {
		if err := prepareTargetSecrets(ctx, input.connection, input.target, input.values.Settings); err != nil {
			return state, fmt.Errorf("prepare alloy credentials: %w", err)
		}
	}
	// Each concurrent chart owns its Helm repository, registry and content caches.
	releases, err := helm.NewReleaseManager(helm.NewReleaseManagerInput{Connection: input.connection, KubeconfigPath: input.kubeconfigPath})
	if err != nil {
		return state, fmt.Errorf("prepare %s Helm client: %w", definition.Name, err)
	}
	defer releases.Close()
	if definition.Name == "traefik" {
		return input.reconcileTraefik(ctx, releases, definition)
	}
	state, err = reconcileComponent(ctx, releases, definition, input.values, input.upgrade && componentSelected(input.upgradeComponents, definition.Name), input.recoverPending || slices.Contains(input.recoverComponents, definition.Name))
	if err != nil {
		return state, err
	}
	if definition.Name == "tailscale-operator" {
		// Helm installs the ProxyClass CRD before Borq can configure generated proxies.
		client, err := networkClient(input.connection)
		if err == nil {
			err = ensureNetworkResource(ctx, client, networkProxyClass())
		}
		if err != nil {
			state.Status = "failed"
			return state, fmt.Errorf("configure tailscale proxy resources: %w", err)
		}
	}
	if definition.Name == "tailscale-operator" || definition.Name == "external-dns" {
		credentialCtx, cancel := context.WithTimeout(ctx, input.values.Timeout)
		defer cancel()
		if err := refreshNetworkControllerCredentials(credentialCtx, input.connection, definition, input.values.Settings); err != nil {
			state.Status = "failed"
			return state, err
		}
	}
	if definition.Name == "openbao" {
		if err := prepareVault(ctx, input.connection, input.values.Settings); err != nil {
			state.Status = "failed"
			return state, fmt.Errorf("initialize openbao: %w", err)
		}
	}
	return state, nil
}
