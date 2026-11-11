package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

type managementKubeconfig struct {
	path        string
	flagPath    string
	contextName string
}

func selectManagementKubeconfig(path string, cfg Config) (managementKubeconfig, func(), error) {
	path = strings.TrimSpace(path)
	if path == "" && !cfg.kubeconfigFromDefault {
		path = cfg.KubeconfigPath
	}
	// The SDK merges KUBECONFIG files and resolves their relative references just
	// like kubectl. A private snapshot keeps that selection stable for this command.
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = path
	configuration, err := rules.Load()
	if err != nil {
		return managementKubeconfig{}, nil, fmt.Errorf("read management kubeconfig: %w", err)
	}
	if err := clientcmdapi.MinifyConfig(configuration); err != nil {
		return managementKubeconfig{}, nil, errors.New("no valid current Kubernetes context; select one with kubectl config use-context NAME or supply --kubeconfig PATH")
	}
	selection := managementKubeconfig{path: path, flagPath: path, contextName: configuration.CurrentContext}
	if path != "" {
		return selection, func() {}, nil
	}
	if err := clientcmdapi.FlattenConfig(configuration); err != nil {
		return managementKubeconfig{}, nil, errors.New("cannot read credentials for the current Kubernetes context; check its certificate and key files")
	}
	encoded, err := clientcmd.Write(*configuration)
	if err != nil {
		return managementKubeconfig{}, nil, errors.New("prepare current Kubernetes context")
	}
	file, err := os.CreateTemp("", "borq-management-kubeconfig-*")
	if err != nil {
		return managementKubeconfig{}, nil, errors.New("create private temporary kubeconfig")
	}
	cleanup := func() { _ = os.Remove(file.Name()) }
	_, writeErr := file.Write(encoded)
	closeErr := file.Close()
	if errors.Join(writeErr, closeErr) != nil {
		cleanup()
		return managementKubeconfig{}, nil, errors.New("save private temporary kubeconfig")
	}
	selection.path = file.Name()
	return selection, cleanup, nil
}

func managementKubeconfigFlag(path string) string {
	if path == "" {
		return ""
	}
	return " --kubeconfig " + quoteLoginArgument(path)
}
