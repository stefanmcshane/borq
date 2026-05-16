package bootstrap

import (
	"bytes"
	"embed"
	"fmt"
	"text/template"
)

//go:embed crds repos.yaml kustomization.yaml flux.yaml gitea.yaml external-secrets.yaml openbao.yaml prometheus.yaml loki.yaml cert-manager.yaml alloy.yaml grafana.yaml
var bootstrapFS embed.FS

type Data struct {
	Namespace   string
	ClusterName string
	SyncUrl     string
	ClusterPath string
}

func Render(name string, data Data) (string, error) {
	raw, err := bootstrapFS.ReadFile(name)
	if err != nil {
		return "", fmt.Errorf("read embedded %q: %w", name, err)
	}
	tmpl, err := template.New(name).Parse(string(raw))
	if err != nil {
		return "", fmt.Errorf("parse template %q: %w", name, err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("execute template %q: %w", name, err)
	}
	return buf.String(), nil
}
