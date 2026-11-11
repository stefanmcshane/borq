package webui

import (
	"context"
	"net/http"
	"net/url"

	"github.com/stefanmcshane/borq/api/appapi"
)

func (web handler) respondOperation(writer http.ResponseWriter, request *http.Request, operation appapi.Operation) {
	selection := request.URL.Query()
	if application := selection.Get("app"); application != "" {
		selected, err := web.applicationOperation(request.Context(), operation, selection.Get("organization"), selection.Get("space"), application)
		if err != nil {
			web.problem(writer, request, err)
			return
		}
		operation = selected
	}
	web.respond(writer, request, "ui-operation", operation, http.StatusOK)
}

func (web handler) applicationOperation(ctx context.Context, operation appapi.Operation, organization, space, application string) (appapi.Operation, error) {
	if len(operation.Resources) == 0 {
		return operation, nil
	}
	plan, err := requestApi[appapi.Plan](ctx, web.api, http.MethodGet, "/api/v1/plans/"+url.PathEscape(operation.PlanID), nil)
	if err != nil {
		return appapi.Operation{}, err
	}
	if plan.Space.ID != operation.SpaceID || plan.Space.Organization != organization || plan.Space.Name != space {
		return appapi.Operation{}, apiProblem{status: http.StatusNotFound, message: "The deployment does not belong to this space."}
	}
	type resourceIdentity struct {
		apiVersion, kind, namespace, name string
	}
	resources := map[resourceIdentity]bool{}
	for _, manifest := range plan.Manifests {
		metadata, _ := manifest["metadata"].(map[string]any)
		labels, _ := metadata["labels"].(map[string]any)
		if labels["app.kubernetes.io/name"] != application {
			continue
		}
		apiVersion, _ := manifest["apiVersion"].(string)
		kind, _ := manifest["kind"].(string)
		namespace, _ := metadata["namespace"].(string)
		name, _ := metadata["name"].(string)
		resources[resourceIdentity{apiVersion, kind, namespace, name}] = true
	}
	// Resource names alone cannot identify an app: routes and predeploy Jobs can
	// have different names. The saved plan retains their labels across releases.
	selected := make([]appapi.ResourceStatus, 0)
	message, state := "", "succeeded"
	for _, resource := range operation.Resources {
		if !resources[resourceIdentity{resource.APIVersion, resource.Kind, resource.Namespace, resource.Name}] {
			continue
		}
		selected = append(selected, resource)
		if resource.State == "failed" || resource.State == "running" && state == "succeeded" {
			state, message = resource.State, resource.Kind+" "+resource.Name+": "+resource.Message
		}
	}
	// A ready app must not turn a failed space deployment into a successful one.
	operation.Resources = selected
	if message != "" {
		operation.Message = message
		return operation, nil
	}
	operation.Message = "Waiting for deployment resources for " + application + "."
	if len(selected) > 0 {
		operation.Message = "All resources for " + application + " are ready."
	}
	if operation.Status != appapi.Succeeded {
		operation.Message += " Space deployment: " + string(operation.Status) + "."
	}
	return operation, nil
}
