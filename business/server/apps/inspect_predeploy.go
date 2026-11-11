package apps

import (
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

func inspectDisabledPredeploy(annotation string) (Predeploy, error) {
	task, err := inspectPredeploySettings(annotation)
	if err != nil {
		return Predeploy{}, err
	}
	if task.Command != "" {
		return Predeploy{}, fmt.Errorf("disabled-predeploy annotation cannot enable a missing Job")
	}
	return task, nil
}

func inspectPredeploySettings(annotation string) (Predeploy, error) {
	task := Predeploy{TimeoutSeconds: 300}
	if annotation == "" {
		return task, nil
	}
	if !strings.HasPrefix(strings.TrimSpace(annotation), "{") {
		return Predeploy{}, fmt.Errorf("predeploy annotation must be a JSON object")
	}
	decoder := json.NewDecoder(strings.NewReader(annotation))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&task); err != nil {
		return Predeploy{}, fmt.Errorf("invalid predeploy annotation: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return Predeploy{}, fmt.Errorf("predeploy annotation must contain one JSON object")
	}
	return task, validatePredeploy(task)
}

func inspectPredeploy(resource inspectedResource, application App, environment inspectedEnvironment, parent inspectedResource) (Predeploy, error) {
	job := batchv1.Job{}
	if err := decodeManifest(resource.manifest, &job); err != nil {
		return Predeploy{}, err
	}
	if job.Labels["borq.run/phase"] != "predeploy" || job.Labels["borq.run/predeploy-name"] != "pre" ||
		job.Spec.Template.Labels["borq.run/phase"] != "predeploy" ||
		job.Spec.Template.Labels["borq.run/predeploy-name"] != "pre" ||
		job.Spec.Template.Labels["borq.run/app-name"] != application.Name ||
		job.Spec.Template.Labels["borq.run/target-id"] != resource.target {
		return Predeploy{}, fmt.Errorf("Job and pod template must identify the predeploy task, application and target")
	}
	if job.Spec.Template.Labels["app.kubernetes.io/name"] != "" {
		return Predeploy{}, fmt.Errorf("predeploy pods must not match application Service selectors")
	}
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds < 1 ||
		*job.Spec.ActiveDeadlineSeconds > 2147483647 {
		return Predeploy{}, fmt.Errorf("predeploy Job requires a positive bounded activeDeadlineSeconds")
	}
	if job.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyNever ||
		(job.Spec.Completions != nil && *job.Spec.Completions != 1) ||
		(job.Spec.Parallelism != nil && *job.Spec.Parallelism != 1) ||
		(job.Spec.Suspend != nil && *job.Spec.Suspend) ||
		(job.Spec.CompletionMode != nil && *job.Spec.CompletionMode != batchv1.NonIndexedCompletion) {
		return Predeploy{}, fmt.Errorf("predeploy Jobs must run one finite process with restartPolicy Never")
	}
	container, found := corev1.Container{}, false
	for _, candidate := range job.Spec.Template.Spec.Containers {
		if candidate.Name != application.Name {
			continue
		}
		if found {
			return Predeploy{}, fmt.Errorf("duplicate predeploy application container")
		}
		container, found = candidate, true
	}
	if !found || container.Image != application.Image {
		return Predeploy{}, fmt.Errorf("predeploy container must use the parent application's image")
	}
	if len(container.Ports) != 0 || container.ReadinessProbe != nil ||
		container.StartupProbe != nil || container.LivenessProbe != nil {
		return Predeploy{}, fmt.Errorf("predeploy container ports and probes are not supported")
	}
	settings, variables, err := inspectSqlite(job.Spec.Template.Spec, container, false)
	if err != nil {
		return Predeploy{}, err
	}
	if settings != application.Sqlite {
		return Predeploy{}, fmt.Errorf("predeploy SQLite storage must match its parent application")
	}
	if len(variables) != 0 {
		return Predeploy{}, fmt.Errorf("predeploy inline environment values are not supported")
	}
	deployment := appsv1.Deployment{}
	if err := decodeManifest(parent.manifest, &deployment); err != nil {
		return Predeploy{}, err
	}
	for _, parentContainer := range deployment.Spec.Template.Spec.Containers {
		if parentContainer.Name == application.Name && !reflect.DeepEqual(container.EnvFrom, parentContainer.EnvFrom) {
			return Predeploy{}, fmt.Errorf("predeploy environment references must match its parent application")
		}
	}
	command, err := inspectCommand(container.Command, container.Args)
	if err != nil {
		return Predeploy{}, fmt.Errorf("command: %w", err)
	}
	if command == "" {
		return Predeploy{}, fmt.Errorf("predeploy Job must specify its command")
	}
	task := Predeploy{
		Command:        command,
		TimeoutSeconds: int(*job.Spec.ActiveDeadlineSeconds), BackoffLimit: 6,
	}
	if job.Spec.BackoffLimit != nil {
		task.BackoffLimit = int(*job.Spec.BackoffLimit)
	}
	return task, validatePredeploy(task)
}
