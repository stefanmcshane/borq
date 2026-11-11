package apps

import (
	"context"
	"fmt"

	"k8s.io/client-go/tools/clientcmd"
)

func validateClusterCredentialAuthority(ctx context.Context, input ClusterInput) error {
	principal, ok := PrincipalFromContext(ctx)
	if !ok || principal.Admin {
		return nil
	}
	denied := fmt.Errorf("%w: organization accounts must supply a self-contained kubeconfig with static credentials; server files, in-cluster credentials and credential plugins require installation administrator access", ErrForbidden)
	if input.InCluster || input.KubeconfigPath != "" || input.Kubeconfig == "" {
		return denied
	}
	configuration, err := clientcmd.Load([]byte(input.Kubeconfig))
	if err != nil {
		return fmt.Errorf("%w: invalid kubeconfig", ErrInvalid)
	}
	// client-go can execute credential plugins or read referenced files before
	// contacting Kubernetes. Organization grants never authorize host access.
	for _, auth := range configuration.AuthInfos {
		if auth.Exec != nil || auth.AuthProvider != nil || auth.TokenFile != "" || auth.ClientCertificate != "" || auth.ClientKey != "" {
			return denied
		}
	}
	for _, cluster := range configuration.Clusters {
		if cluster.CertificateAuthority != "" || cluster.ProxyURL != "" {
			return denied
		}
	}
	if input.Context != "" {
		configuration.CurrentContext = input.Context
	}
	// An empty kubeconfig makes client-go fall back to the pod's credentials.
	// Validate the selected context and API server before allowing that loader.
	if err := clientcmd.Validate(*configuration); err != nil {
		return denied
	}
	selected := configuration.Contexts[configuration.CurrentContext]
	if selected == nil {
		return denied
	}
	credentials := configuration.AuthInfos[selected.AuthInfo]
	if credentials == nil || !(credentials.Token != "" || len(credentials.ClientCertificateData) > 0 && len(credentials.ClientKeyData) > 0 || credentials.Username != "" && credentials.Password != "") {
		return denied
	}
	return nil
}
