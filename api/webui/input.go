package webui

import (
	"encoding/json"
	"net/url"
	"strings"

	"github.com/stefanmcshane/borq/api/appapi"
)

func trimFormFields(form url.Values) {
	for name, values := range form {
		switch name {
		case "name", "image", "port", "replicas", "organization", "space", "cluster",
			"app", "application", "postgres", "spacePocketId", "issuerUrl", "adminUsername", "adminEmail", "storage", "storageClass", "certificateIssuer", "plan", "deployment", "backup", "group",
			"sourceName", "sourceVersion", "version", "baseVersion", "groupVersion",
			"environment", "environmentVersion", "environmentChoice", "variableName",
			"ingressName", "ingressHost", "ingressSubdomain", "ingressDomain", "ingressPath", "savedIngress", "serviceNamespace",
			"readinessPath", "readinessPort", "readinessPeriodSeconds", "readinessTimeoutSeconds", "readinessFailureThreshold",
			"startupPath", "startupPort", "startupPeriodSeconds", "startupTimeoutSeconds", "startupFailureThreshold",
			"predeployTimeoutSeconds", "predeployBackoffLimit", "removeIngress",
			"repository", "repo", "chart", "region", "bucket", "prefix", "endpoint", "interval",
			"keepLast", "githubUsername", "recoveryTarget", "component", "componentScope",
			"componentOrganization", "componentCluster", "managementOperation", "view", "tab", "subtab", "node", "range", "end":
			for index, value := range values {
				values[index] = strings.TrimSpace(value)
			}
		}
	}
}

func (web handler) trimInputDocument(document json.RawMessage, schema string) (json.RawMessage, error) {
	var value any
	if err := json.Unmarshal(document, &value); err != nil {
		return nil, err
	}
	appapi.NormalizeInput(value, web.inputSchemas[schema].Value)
	return json.Marshal(value)
}
