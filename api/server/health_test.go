package server

import (
	"context"
	"testing"

	"github.com/matryer/is"
)

func TestHealthChecksRunAllChecks(t *testing.T) {
	is := is.New(t)

	events := make([]string, 0, 2)
	checks := []HealthCheck{
		{
			Name: "first",
			Run: func(context.Context) HealthResult {
				events = append(events, "first")
				return HealthResult{Name: "first", Status: healthStatusFail, Description: "failed"}
			},
		},
		{
			Name: "second",
			Run: func(context.Context) HealthResult {
				events = append(events, "second")
				return HealthResult{Name: "second", Status: healthStatusSuccess, Description: "passed"}
			},
		},
	}

	report := runHealthChecks(context.Background(), checks)

	is.Equal(events, []string{"first", "second"})
	is.Equal(report.Status, healthStatusFail)
	is.Equal(len(report.Checks), 2)
	is.Equal(report.Checks[0].Name, "first")
	is.Equal(report.Checks[0].Status, healthStatusFail)
	is.Equal(report.Checks[0].Description, "failed")
	is.Equal(report.Checks[1].Name, "second")
	is.Equal(report.Checks[1].Status, healthStatusSuccess)
	is.Equal(report.Checks[1].Description, "passed")
}

func TestHealthChecksSucceedWhenAllChecksSucceed(t *testing.T) {
	is := is.New(t)

	checks := []HealthCheck{
		{
			Name: "first",
			Run: func(context.Context) HealthResult {
				return HealthResult{Name: "first", Status: healthStatusSuccess, Description: "passed"}
			},
		},
	}

	report := runHealthChecks(context.Background(), checks)

	is.Equal(report.Status, healthStatusSuccess)
	is.Equal(len(report.Checks), 1)
}

func TestHealthReportFailureDescription(t *testing.T) {
	is := is.New(t)

	report := HealthReport{
		Status: healthStatusFail,
		Checks: []HealthResult{
			{Name: "first", Status: healthStatusFail, Description: "denied"},
			{Name: "second", Status: healthStatusSuccess, Description: "ok"},
			{Name: "third", Status: healthStatusFail, Description: "timeout"},
		},
	}

	is.Equal(report.failureDescription(), "first: denied; third: timeout")
}
