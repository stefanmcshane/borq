package webui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/stefanmcshane/borq/api/appapi"
)

func applicationFields(form url.Values) (appapi.AppPatch, error) {
	var patch appapi.AppPatch
	if source := form.Get("document"); source != "" {
		if err := json.Unmarshal([]byte(source), &patch); err != nil {
			return patch, apiProblem{status: http.StatusBadRequest, message: "Reload the application editor and try again."}
		}
	}
	port, portError := strconv.Atoi(form.Get("port"))
	replicas, replicasError := strconv.Atoi(form.Get("replicas"))
	if portError != nil || replicasError != nil || port < 0 || port > 65535 || replicas < 0 || int64(replicas) > 2147483647 {
		return patch, apiProblem{status: http.StatusBadRequest, message: "Port must be between 0 and 65535; replicas must be between 0 and 2147483647."}
	}
	image, command := form.Get("image"), form.Get("command")
	patch.Name, patch.Image, patch.Command = form.Get("name"), &image, &command
	patch.Port, patch.Replicas = &port, &replicas
	name, version := form.Get("environment"), form.Get("environmentVersion")
	if name != "" || version != "" {
		// API pointers preserve an omitted group name when only its version changes.
		patch.Env = &appapi.EnvReferencePatch{}
		if name != "" {
			patch.Env.Name = &name
		}
		if version != "" {
			patch.Env.Version = &version
		}
	}
	if patch.Checks == nil {
		patch.Checks = &appapi.ChecksPatch{}
	}
	readiness, err := applicationCheck(form, "readiness", patch.Checks.Readiness)
	if err != nil {
		return patch, err
	}
	startup, err := applicationCheck(form, "startup", patch.Checks.Startup)
	if err != nil {
		return patch, err
	}
	patch.Checks.Readiness, patch.Checks.Startup = &readiness, &startup
	// The guided form describes the entire optional task. An absent or cleared
	// command must send an explicit empty string instead of retaining the saved task.
	if patch.Predeploy == nil {
		patch.Predeploy = &appapi.PredeployPatch{}
	}
	predeployCommand := form.Get("predeployCommand")
	patch.Predeploy.Command = &predeployCommand
	patch.Predeploy.TimeoutSeconds, err = applicationInteger(form, "predeployTimeoutSeconds", "Predeploy timeout", 1, 2147483647, patch.Predeploy.TimeoutSeconds)
	if err != nil {
		return patch, err
	}
	patch.Predeploy.BackoffLimit, err = applicationInteger(form, "predeployBackoffLimit", "Predeploy retries", 0, 2147483647, patch.Predeploy.BackoffLimit)
	if err != nil {
		return patch, err
	}
	if form.Has("ingressFields") {
		names, hosts, paths := form["ingressName"], form["ingressHost"], form["ingressPath"]
		if len(names) != len(hosts) || len(names) != len(paths) {
			return patch, apiProblem{status: http.StatusBadRequest, message: "Each route needs a name, host, and path field."}
		}
		ingresses := make([]appapi.Ingress, 0, len(names))
		for index, name := range names {
			path := paths[index]
			ingresses = append(ingresses, appapi.Ingress{Name: name, Host: hosts[index], Path: &path})
		}
		patch.Ingresses = &ingresses
	}
	return patch, nil
}

// Generated patch pointers distinguish omitted probe fields from explicit false and zero.
func applicationCheck(form url.Values, name string, current *appapi.CheckPatch) (appapi.CheckPatch, error) {
	check := appapi.CheckPatch{}
	if current != nil {
		check = *current
	}
	enabled, path := form.Get(name) == "on", form.Get(name+"Path")
	check.Enabled, check.Path = &enabled, &path
	var err error
	check.Port, err = applicationInteger(form, name+"Port", name+" port", 0, 65535, check.Port)
	if err != nil {
		return check, err
	}
	check.PeriodSeconds, err = applicationInteger(form, name+"PeriodSeconds", name+" interval", 1, 2147483647, check.PeriodSeconds)
	if err != nil {
		return check, err
	}
	check.TimeoutSeconds, err = applicationInteger(form, name+"TimeoutSeconds", name+" timeout", 1, 2147483647, check.TimeoutSeconds)
	if err != nil {
		return check, err
	}
	check.FailureThreshold, err = applicationInteger(form, name+"FailureThreshold", name+" failure threshold", 1, 2147483647, check.FailureThreshold)
	return check, err
}

// Optional API integers use pointers so omission and an explicit zero remain distinct.
func applicationInteger(form url.Values, name, label string, minimum, maximum int, current *int) (*int, error) {
	if form.Get(name) == "" {
		return current, nil
	}
	value, err := strconv.Atoi(form.Get(name))
	if err != nil || value < minimum || value > maximum {
		return nil, apiProblem{status: http.StatusBadRequest, message: fmt.Sprintf("%s must be a whole number between %d and %d.", label, minimum, maximum)}
	}
	return &value, nil
}
