package server

import (
	"context"
	"fmt"
	"strings"

	"github.com/stefanmcshane/borq/foundations/kubernetes"
)

// HealthCheck runs one repeatable server healthcheck.
type HealthCheck struct {
	Name string
	Run  func(context.Context) HealthResult
}

// HealthResult describes the outcome of one server healthcheck.
type HealthResult struct {
	Name        string `json:"name"`
	Status      string `json:"status"`
	Description string `json:"description"`
}

// HealthReport describes the full server health state.
type HealthReport struct {
	Status string         `json:"status"`
	Checks []HealthResult `json:"checks"`
}

func runHealthChecks(ctx context.Context, checks []HealthCheck) HealthReport {
	results := make([]HealthResult, 0, len(checks))
	overallStatus := healthStatusSuccess

	for _, check := range checks {
		result := check.Run(ctx)
		if result.Name == "" {
			result.Name = check.Name
		}
		if result.Status != healthStatusSuccess {
			result.Status = healthStatusFail
			overallStatus = healthStatusFail
		}
		results = append(results, result)
	}

	return HealthReport{
		Status: overallStatus,
		Checks: results,
	}
}

func (report HealthReport) failureDescription() string {
	descriptions := make([]string, 0, len(report.Checks))
	for _, check := range report.Checks {
		if check.Status == healthStatusFail {
			descriptions = append(descriptions, fmt.Sprintf("%s: %s", check.Name, check.Description))
		}
	}

	return strings.Join(descriptions, "; ")
}

func newKubernetesAuthHealthCheck(connection kubernetes.Connection, namespace string) HealthCheck {
	return HealthCheck{
		Name: kubernetesAuthCheckName,
		Run: func(ctx context.Context) HealthResult {
			err := connection.SelfSubjectAccessReview(ctx, kubernetes.SelfSubjectAccessReviewInput{
				Namespace: namespace,
				Rule: kubernetes.PolicyRule{
					APIGroups: []string{"*"},
					Resources: []string{"*"},
					Verbs:     []string{"*"},
				},
			})
			if err != nil {
				return HealthResult{
					Name:        kubernetesAuthCheckName,
					Status:      healthStatusFail,
					Description: fmt.Sprintf("credentials cannot manage resources in namespace %q: %v", namespace, err),
				}
			}

			return HealthResult{
				Name:        kubernetesAuthCheckName,
				Status:      healthStatusSuccess,
				Description: fmt.Sprintf("credentials can manage resources in namespace %q", namespace),
			}
		},
	}
}
