package apps

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

func normalizeCertificateIssuer(name string) (string, error) {
	if name == "" {
		return "borq", nil
	}
	if problems := validation.IsDNS1123Subdomain(name); len(problems) != 0 {
		return "", fmt.Errorf("invalid certificate issuer %q: %s", name, strings.Join(problems, "; "))
	}
	return name, nil
}

func ingressSecretName(resourceName string) string {
	// App names are bounded to 50 characters and route names to 63, so this
	// stays below the Secret's 253-character DNS subdomain limit without truncation.
	return resourceName + "-tls"
}
