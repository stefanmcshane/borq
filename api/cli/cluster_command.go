package cli

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/stefanmcshane/borq/api/appapi"
	localcluster "github.com/stefanmcshane/borq/business/cli/cluster"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

type clusterLinkOptions struct {
	scope                                         applicationScope
	kubeconfig, localName, contextName, apiServer string
	reconcile, diff                               bool
	output                                        string
	verbose                                       bool
	file                                          string
	tailscale                                     *appapi.ClusterTailscale
}

type clusterLinkCredentials struct {
	path, localName string
}

func newClusterCommand() *cobra.Command {
	group := &cobra.Command{Use: "cluster", Short: "Link existing Kubernetes clusters as deployment targets.", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, arguments []string) error { return command.Help() },
	}
	group.AddCommand(newClusterLinkCommand(), newClusterListCommand(), newClusterKubeconfigCommand())
	group.AddCommand(newClusterComponentsCommands()...)
	return group
}

func newClusterLinkCommand() *cobra.Command {
	options := clusterLinkOptions{reconcile: true, diff: true, output: "cli"}
	link := &cobra.Command{Use: "link [NAME]", Short: "Link a cluster as NAME-xxxxx, create its initial space, and queue setup.", Args: cobra.MaximumNArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			if command.Flags().Changed("file") && options.file == "" {
				return errors.New("--file must name a cluster YAML document")
			}
			if options.output != "cli" && options.output != "json" && options.output != "yaml" {
				return errors.New("--output must be cli, json, or yaml")
			}
			var name string
			if options.file != "" {
				selected, cleanup, err := prepareClusterLinkFile(command, &options, arguments)
				if err != nil {
					return err
				}
				defer cleanup()
				name = selected
			}
			if err := prepareApplicationScope(command, &options.scope); err != nil {
				return err
			}
			if options.file == "" {
				selected, err := clusterArgument(arguments, options.scope.cluster)
				if err != nil {
					return err
				}
				name = selected
			}
			return executeClusterLink(command, options, name)
		},
	}
	addOrganizationScopeFlags(link, &options.scope)
	// YAML can supply the organization, so resolve profile defaults after reading it.
	link.PreRunE = nil
	link.Flags().StringVar(&options.output, "output", "cli", "Output format: cli, json, or yaml.")
	link.Flags().BoolVar(&options.verbose, "verbose", false, "Show full component values diffs.")
	link.Flags().StringVarP(&options.file, "file", "f", "", "Cluster YAML with an exact name, kubeconfigPath and optional Tailscale credentials; creates or updates that registration without an implicit space.")
	link.Flags().StringVar(&options.kubeconfig, "kubeconfig", "", "Explicit kubeconfig to upload; omit to use this local cluster's Borq-managed credentials.")
	link.Flags().StringVar(&options.localName, "local", "", "Existing saved local cluster to link under NAME; defaults to NAME.")
	link.Flags().StringVar(&options.contextName, "context", "", "Context within the supplied kubeconfig.")
	link.Flags().StringVar(&options.apiServer, "api-server", "", "Advanced override for the Kubernetes API URL in the upload; leaves the local file unchanged.")
	_ = link.Flags().MarkHidden("api-server")
	link.Flags().BoolVar(&options.reconcile, "reconcile", true, "Queue shared component setup; false only saves the cluster registration.")
	link.Flags().BoolVar(&options.diff, "diff", true, "Preview component changes before enqueueing; false skips the read-only comparison.")
	link.MarkFlagsMutuallyExclusive("kubeconfig", "local", "file")
	return link
}

type clusterLinkFile struct {
	Name           string                   `json:"name"`
	Organization   string                   `json:"organization,omitempty"`
	KubeconfigPath string                   `json:"kubeconfigPath,omitempty"`
	Kubeconfig     string                   `json:"kubeconfig,omitempty"`
	Context        string                   `json:"context,omitempty"`
	Tailscale      *appapi.ClusterTailscale `json:"tailscale,omitempty"`
}

func prepareClusterLinkFile(command *cobra.Command, options *clusterLinkOptions, arguments []string) (string, func(), error) {
	cleanup := func() {}
	var input clusterLinkFile
	if err := readManagedDocument(options.file, "ClusterLinkFile", &input); err != nil {
		return "", cleanup, errors.New("invalid cluster YAML; check field names and value types")
	}
	if input.Name == "" {
		return "", cleanup, errors.New("cluster YAML requires an exact name")
	}
	if len(arguments) != 0 && arguments[0] != input.Name {
		return "", cleanup, errors.New("cluster argument must match the name in the YAML file")
	}
	if input.Organization != "" {
		if command.Flags().Changed("organization") && options.scope.organization != input.Organization {
			return "", cleanup, errors.New("organization flag must match the cluster YAML")
		}
		if err := command.Flags().Set("organization", input.Organization); err != nil {
			return "", cleanup, err
		}
	}
	if !command.Flags().Changed("context") {
		options.contextName = input.Context
	}
	options.tailscale = input.Tailscale
	if input.Kubeconfig != "" && input.KubeconfigPath != "" {
		return "", cleanup, errors.New("cluster YAML accepts kubeconfig or kubeconfigPath, not both")
	}
	if input.Kubeconfig != "" {
		file, err := os.CreateTemp("", "borq-cluster-*.yaml")
		if err != nil {
			return "", cleanup, errors.New("create private cluster upload")
		}
		cleanup = func() { _ = os.Remove(file.Name()) }
		_, writeErr := file.WriteString(input.Kubeconfig)
		closeErr := file.Close()
		if errors.Join(writeErr, closeErr) != nil {
			cleanup()
			return "", func() {}, errors.New("prepare private cluster upload")
		}
		options.kubeconfig = file.Name()
	} else if input.KubeconfigPath != "" {
		options.kubeconfig = input.KubeconfigPath
		if !filepath.IsAbs(options.kubeconfig) {
			options.kubeconfig = filepath.Join(filepath.Dir(options.file), options.kubeconfig)
		}
	} else {
		return "", cleanup, errors.New("cluster YAML requires kubeconfig or kubeconfigPath")
	}
	return input.Name, cleanup, nil
}

func clusterArgument(arguments []string, current string) (string, error) {
	if len(arguments) == 1 {
		return arguments[0], nil
	}
	if current == "" {
		return "", errors.New("provide a cluster name or set a default with borq config set --cluster NAME")
	}
	return current, nil
}

func newClusterListCommand() *cobra.Command {
	listScope := applicationScope{}
	list := &cobra.Command{Use: "list", Short: "List clusters linked to an organization.", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, arguments []string) error { return executeClusterList(command, listScope) },
	}
	addOrganizationScopeFlags(list, &listScope)
	return list
}

// The pointer lets Cobra populate command-local scope without shared state.
func addOrganizationScopeFlags(command *cobra.Command, scope *applicationScope) {
	addApplicationServerFlag(command, scope)
	command.Flags().StringVar(&scope.organization, "organization", "", "Organization owning this resource.")
}

func executeClusterLink(command *cobra.Command, options clusterLinkOptions, name string) error {
	credentials, err := resolveClusterLinkCredentials(options, name)
	if err != nil {
		return err
	}
	return executeClusterLinkRegistration(command, options, name, credentials)
}

func resolveClusterLinkCredentials(options clusterLinkOptions, name string) (clusterLinkCredentials, error) {
	if options.kubeconfig != "" {
		if options.localName != "" {
			return clusterLinkCredentials{}, errors.New("--kubeconfig cannot be combined with --local")
		}
		return clusterLinkCredentials{path: options.kubeconfig}, nil
	}
	localName := options.localName
	if localName == "" {
		localName = name
	}
	local, err := localcluster.GetLocal(localName)
	if err != nil {
		return clusterLinkCredentials{}, err
	}
	if local.Organization != "" && local.Organization != options.scope.organization {
		return clusterLinkCredentials{}, errors.New("local cluster belongs to a different organization; select its organization when linking")
	}
	return clusterLinkCredentials{path: local.KubeconfigPath, localName: localName}, nil
}

// Cobra requires the command pointer to retain its context and output streams.
func executeClusterLinkRegistration(command *cobra.Command, options clusterLinkOptions, name string, credentials clusterLinkCredentials) error {
	configuration, err := clientcmd.LoadFromFile(credentials.path)
	if err != nil {
		if credentials.localName != "" && errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("local cluster %q has no saved credentials; provide --kubeconfig for an existing cluster", credentials.localName)
		}
		if options.file != "" {
			return errors.New("read kubeconfig supplied by the cluster YAML")
		}
		return fmt.Errorf("read cluster kubeconfig: %w", err)
	}
	if err := selectClusterLinkEndpoint(configuration, options.contextName, ""); err != nil {
		return err
	}
	contextName := options.contextName
	if contextName == "" {
		contextName = configuration.CurrentContext
	}
	clientApiServer := configuration.Clusters[configuration.Contexts[contextName].Cluster].Server
	apiServer := options.apiServer
	if credentials.localName != "" && apiServer == "" {
		apiServer, err = localcluster.ResolveLocalApiServer(command.Context(), credentials.path, options.contextName)
		if err != nil {
			return fmt.Errorf("find local cluster %q Kubernetes API address: %w; start the cluster or provide --api-server", credentials.localName, err)
		}
	}
	if err := selectClusterLinkEndpoint(configuration, options.contextName, apiServer); err != nil {
		return err
	}
	if err := clientcmdapi.FlattenConfig(configuration); err != nil {
		return fmt.Errorf("embed cluster credentials: %w", err)
	}
	encoded, err := clientcmd.Write(*configuration)
	if err != nil {
		return fmt.Errorf("encode cluster kubeconfig: %w", err)
	}
	client, err := applicationClient(options.scope.server)
	if err != nil {
		return err
	}
	source := string(encoded)
	// Registration precedes previews so a later setup failure remains inspectable.
	reconcile := false
	body := appapi.ClusterInput{Context: &options.contextName, Kubeconfig: &source, ClientApiServer: &clientApiServer, Reconcile: &reconcile, Tailscale: options.tailscale}
	var linked *appapi.Cluster
	if options.file != "" {
		response, err := client.PutClusterWithResponse(command.Context(), options.scope.organization, name, body)
		if err != nil {
			return err
		}
		if response.JSON200 == nil {
			return applicationFailure(response.StatusCode(), response.Body)
		}
		linked = response.JSON200
	} else {
		response, err := client.LinkClusterWithResponse(command.Context(), options.scope.organization, appapi.LinkClusterInput{Name: name, Connection: body})
		if err != nil {
			return err
		}
		if response.JSON200 == nil {
			return applicationFailure(response.StatusCode(), response.Body)
		}
		linked = response.JSON200
	}
	name = linked.Name
	if options.diff {
		preview, err := client.DiffClusterComponentsWithResponse(command.Context(), options.scope.organization, name, &appapi.DiffClusterComponentsParams{})
		if err != nil {
			_ = printClusterLinkValue(command, options, linked)
			return fmt.Errorf("cluster %s is linked; preview failed: %w", name, err)
		}
		if preview.JSON200 == nil {
			state, stateErr := client.GetClusterComponentsWithResponse(command.Context(), options.scope.organization, name)
			if preview.StatusCode() == http.StatusBadRequest && stateErr == nil && state.JSON200 != nil && state.JSON200.Status == "disabled" && state.JSON200.Message == "infrastructure management is disabled" {
				_, _ = fmt.Fprintln(command.ErrOrStderr(), "Cluster linked; infrastructure management is disabled, so no components were scheduled.")
				return printClusterLinkValue(command, options, linked)
			}
			_ = printClusterLinkValue(command, options, linked)
			return fmt.Errorf("cluster %s is linked; preview failed: %w", name, applicationFailure(preview.StatusCode(), preview.Body))
		}
		if err := printComponentDiff(command.ErrOrStderr(), *preview.JSON200, options.verbose); err != nil {
			return err
		}
		for _, component := range preview.JSON200.Components {
			if component.Action == "conflict" {
				_ = printClusterLinkValue(command, options, linked)
				return fmt.Errorf("cluster %s is linked; component %s conflicts with an existing installation: %s", name, component.Name, component.Reason)
			}
		}
	}
	if !options.reconcile {
		if options.output == "cli" {
			_, err := fmt.Fprintf(command.OutOrStdout(), "Cluster %s linked to %s; component setup was not scheduled.\n", name, options.scope.organization)
			return err
		}
		return printClusterLinkValue(command, options, linked)
	}
	queued, err := client.ReconcileClusterComponentsWithResponse(command.Context(), options.scope.organization, name)
	if err != nil {
		_ = printClusterLinkValue(command, options, linked)
		return fmt.Errorf("cluster %s is linked; enqueue failed: %w", name, err)
	}
	if queued.JSON200 == nil {
		_ = printClusterLinkValue(command, options, linked)
		return fmt.Errorf("cluster %s is linked; enqueue failed: %w", name, applicationFailure(queued.StatusCode(), queued.Body))
	}
	if options.output == "cli" {
		_, err := fmt.Fprintf(command.OutOrStdout(), "Cluster %s linked to %s; target components %s.\n", name, options.scope.organization, queued.JSON200.Status)
		return err
	}
	return printClusterLinkValue(command, options, queued.JSON200)
}

func executeClusterList(command *cobra.Command, scope applicationScope) error {
	client, err := applicationClient(scope.server)
	if err != nil {
		return err
	}
	response, err := client.ListClustersWithResponse(command.Context(), scope.organization)
	if err != nil {
		return err
	}
	if response.JSON200 == nil {
		return applicationFailure(response.StatusCode(), response.Body)
	}
	return printApplicationValue(command.OutOrStdout(), response.JSON200)
}

// The client-go config pointer is required to update only the selected cluster before serialization.
func selectClusterLinkEndpoint(configuration *clientcmdapi.Config, contextName, apiServer string) error {
	if contextName == "" {
		contextName = configuration.CurrentContext
	}
	selected, ok := configuration.Contexts[contextName]
	if !ok || selected == nil {
		return fmt.Errorf("selected kubeconfig context %q does not exist", contextName)
	}
	cluster, ok := configuration.Clusters[selected.Cluster]
	if !ok || cluster == nil {
		return fmt.Errorf("selected kubeconfig context %q has no cluster", contextName)
	}
	if apiServer == "" {
		return nil
	}
	endpoint, err := url.Parse(apiServer)
	if err != nil || endpoint.Hostname() == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.ForceQuery || endpoint.Fragment != "" {
		return errors.New("--api-server must be an absolute HTTP(S) URL without credentials, a query, or a fragment")
	}
	cluster.Server = apiServer
	return nil
}
