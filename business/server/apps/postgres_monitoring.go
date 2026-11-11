package apps

import (
	"context"
	"errors"
	"fmt"
	"github.com/stefanmcshane/borq/foundations/postgres"
	"strconv"
	"strings"
	"time"

	"github.com/stefanmcshane/borq/foundations/observability"
)

func (manager Manager) postgresTelemetry(ctx context.Context, input TelemetryInput) (postgresRecord, string, error) {
	stored, _, err := loadDocument[postgresRecord](ctx, manager.database, "postgres", addonKey(input.Organization, input.Space, input.Application))
	if err != nil {
		return stored, "", err
	}
	if stored.Postgres.State == "removed" {
		return stored, "", ErrNotFound
	}
	target, err := manager.GetSpace(ctx, input.Organization, input.Space)
	if err != nil {
		return stored, "", err
	}
	selector := fmt.Sprintf("{borq_cluster=%s,namespace=%s,borq_space=%s,borq_addon=%s}", strconv.Quote(stableId(input.Organization+"/"+target.Cluster)), strconv.Quote(stored.Postgres.Namespace), strconv.Quote(target.ID), strconv.Quote(stored.Postgres.ClusterName))
	return stored, selector, nil
}

// GetPostgresMetrics combines scoped CPU/memory history, real SQL activity and filesystem space.
func (manager Manager) GetPostgresMetrics(ctx context.Context, input TelemetryInput) (PostgresMetrics, error) {
	stored, selector, err := manager.postgresTelemetry(ctx, input)
	if err != nil {
		return PostgresMetrics{}, err
	}
	result := PostgresMetrics{Protection: stored.Postgres.Protection}
	result.Usage, err = manager.workloadMetrics(ctx, input, strings.TrimSuffix(selector, "}")+`,job="borq-cadvisor"}`)
	if err != nil {
		if errors.Is(err, ErrInvalid) {
			return result, err
		}
		result.Notice = "CPU and memory history unavailable"
	}
	client, clusterInput, err := manager.postgresClient(ctx, stored, false)
	if err != nil {
		return result, err
	}
	stats, err := client.Statistics(ctx, clusterInput)
	if err != nil {
		result.Notice = strings.Trim(result.Notice+"; live PostgreSQL statistics unavailable", "; ")
		if postgresBackupsEnabled(stored.Postgres.Specification) {
			result.Protection.Status = "degraded"
			result.Protection.Message = "live PostgreSQL protection could not be observed"
		} else {
			result.Protection = postgresProtection(stored, postgres.Observation{}, time.Now().UTC())
		}
		return result, nil
	}
	result.ActivityAvailable = true
	result.DiskBytes, result.Connections, result.ActiveQueries, result.Transactions = stats.DatabaseBytes, stats.Connections, stats.ActiveQueries, stats.Transactions
	result.DiskAvailable, result.DiskUsedBytes, result.DiskCapacityBytes, result.DiskAvailableBytes = stats.DiskAvailable, stats.DiskUsedBytes, stats.DiskCapacityBytes, stats.DiskAvailableBytes
	if !stats.DiskAvailable {
		result.Notice = strings.Trim(result.Notice+"; filesystem capacity unavailable", "; ")
	}
	observed, err := client.Observe(ctx, clusterInput)
	if err == nil {
		result.Protection = postgresProtection(stored, observed, time.Now().UTC())
	} else {
		if postgresBackupsEnabled(stored.Postgres.Specification) {
			result.Protection.Status = "degraded"
			result.Protection.Message = "live PostgreSQL protection could not be observed"
		} else {
			result.Protection = postgresProtection(stored, postgres.Observation{}, time.Now().UTC())
		}
	}
	return result, nil
}

// GetPostgresLogs returns bounded scoped persisted logs, with live primary output as a fallback.
func (manager Manager) GetPostgresLogs(ctx context.Context, input TelemetryInput) (Logs, error) {
	stored, selector, err := manager.postgresTelemetry(ctx, input)
	if err != nil {
		return Logs{}, err
	}
	result, err := manager.workloadLogs(ctx, input, selector)
	if err == nil {
		return result, nil
	}
	start, end, _, err := telemetryWindow(input)
	if err != nil {
		return Logs{}, err
	}
	limit := input.Limit
	if limit == 0 {
		limit = 200
	}
	if limit < 1 || limit > 1000 {
		return Logs{}, fmt.Errorf("%w: log limit must be between 1 and 1000", ErrInvalid)
	}
	client, clusterInput, err := manager.postgresClient(ctx, stored, false)
	if err != nil {
		return Logs{}, err
	}
	text, err := client.Logs(ctx, clusterInput, limit, start)
	if err != nil {
		return Logs{}, err
	}
	result = Logs{Start: start, End: end, Events: []ApplicationEvent{}, EventsNotice: "showing live primary logs; persisted history is unavailable", Entries: []observability.Entry{}}
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		stamp, message, ok := strings.Cut(line, " ")
		timestamp, err := time.Parse(time.RFC3339Nano, stamp)
		if !ok || err != nil || timestamp.After(end) || !strings.Contains(message, input.Search) {
			continue
		}
		result.Entries = append(result.Entries, observability.Entry{Timestamp: timestamp, Message: message})
	}
	result.LimitReached = len(result.Entries) >= limit
	return result, nil
}
