package webui

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/stefanmcshane/borq/api/appapi"
)

func (web handler) applicationRoutePolicy(request *http.Request, organization, space, application string) (appapi.ApplicationRoutePolicy, error) {
	path := spacePath(organization, space) + "/route-policy"
	if application != "" {
		path += "?" + url.Values{"application": {application}}.Encode()
	}
	return requestApi[appapi.ApplicationRoutePolicy](request.Context(), web.api, http.MethodGet, path, nil)
}

func (web handler) loadApplicationRoutePolicy(request *http.Request, editor *applicationEditorData) {
	if editor.Organization == "" || editor.Space == "" {
		return
	}
	policy, err := web.applicationRoutePolicy(request, editor.Organization, editor.Space, editor.Draft.Name)
	if err != nil {
		editor.RoutePolicyError = "Public route settings could not be loaded: " + err.Error()
		for index := range editor.Ingresses {
			editor.Ingresses[index].Subdomain = editor.Ingresses[index].Route.Host
		}
		return
	}
	editor.RoutePolicy = policy
	for index := range editor.Ingresses {
		route := &editor.Ingresses[index]
		route.Subdomain = route.Route.Host
		if len(policy.Domains) > 0 {
			route.Domain = policy.Domains[0]
		}
		if route.Route.Host == "" {
			continue
		}
		matched := false
		for _, domain := range policy.Domains {
			label, found := strings.CutSuffix(route.Route.Host, "."+domain)
			if found && applicationSubdomainValid(label) {
				route.Subdomain, route.Domain, matched = label, domain, true
				break
			}
		}
		if !matched {
			route.HostError = "Existing hostname " + route.Route.Host + " is outside this space's eligible domains. Replace it with one subdomain label."
		}
	}
}

func (web handler) applicationRouteFields(request *http.Request, form url.Values) error {
	if !form.Has("ingressSubdomain") {
		return nil
	}
	labels, domains, names := form["ingressSubdomain"], form["ingressDomain"], form["ingressName"]
	if len(labels) != len(domains) || len(labels) != len(names) {
		return apiProblem{status: http.StatusBadRequest, message: "Each public route needs a subdomain and an eligible domain."}
	}
	policy, err := web.applicationRoutePolicy(request, form.Get("organization"), form.Get("space"), form.Get("name"))
	if err != nil {
		return err
	}
	hosts := make([]string, len(labels))
	for index, label := range labels {
		if !slices.Contains(policy.Domains, domains[index]) {
			return apiProblem{status: http.StatusBadRequest, message: "Choose a domain allowed by this space's cluster certificate issuer. Reload the editor if its domains changed."}
		}
		if label == "" && form.Get("action") != "plan-app" {
			continue
		}
		if !applicationSubdomainValid(label) {
			return apiProblem{status: http.StatusBadRequest, message: fmt.Sprintf("Route %q needs one subdomain label, such as web. Use lowercase letters, numbers and hyphens; do not enter a full hostname.", names[index])}
		}
		hosts[index] = label + "." + domains[index]
	}
	form["ingressHost"] = hosts
	return nil
}

func applicationSubdomainValid(label string) bool {
	if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
		return false
	}
	for _, character := range label {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
			return false
		}
	}
	return true
}
