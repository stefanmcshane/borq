package cli

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"

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
			if options.output != "cli" && options.output != "json" && options.output != "yaml" {
				return errors.New("--output must be cli, json, or yaml")
			}
			name, err := clusterArgument(arguments, options.scope.cluster)
			if err != nil {
				return err
			}
			return executeClusterLink(command, options, name)
		},
	}
	addOrganizationScopeFlags(link, &options.scope)
	link.Flags().StringVar(&options.output, "output", "cli", "Output format: cli, json, or yaml.")
	link.Flags().BoolVar(&options.verbose, "verbose", false, "Show full component values diffs.")
	link.Flags().StringVar(&options.kubeconfig, "kubeconfig", "", "Explicit kubeconfig to upload; omit to use this local cluster's Borq-managed credentials.")
	link.Flags().StringVar(&options.localName, "local", "", "Existing saved local cluster to link under NAME; defaults to NAME.")
	link.Flags().StringVar(&options.contextName, "context", "", "Context within the supplied kubeconfig.")
	link.Flags().StringVar(&options.apiServer, "api-server", "", "Advanced override for the Kubernetes API URL in the upload; leaves the local file unchanged.")
	_ = link.Flags().MarkHidden("api-server")
	link.Flags().BoolVar(&options.reconcile, "reconcile", true, "Queue shared component setup; false only saves the cluster registration.")
	link.Flags().BoolVar(&options.diff, "diff", true, "Preview component changes before enqueueing; false skips the read-only comparison.")
	link.MarkFlagsMutuallyExclusive("kubeconfig", "local")
	return link
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
	// Linking registers first so a preview or later setup failure cannot undo it.
	reconcile := false
	body := appapi.ClusterInput{Context: &options.contextName, Kubeconfig: &source, ClientApiServer: &clientApiServer, Reconcile: &reconcile}
	response, err := client.LinkClusterWithResponse(command.Context(), options.scope.organization, appapi.LinkClusterInput{Name: name, Connection: body})
	if err != nil {
		return err
	}
	if response.JSON200 == nil {
		return applicationFailure(response.StatusCode(), response.Body)
	}
	name = response.JSON200.Name
	if options.diff {
		preview, err := client.DiffClusterComponentsWithResponse(command.Context(), options.scope.organization, name, &appapi.DiffClusterComponentsParams{})
		if err != nil {
			_ = printClusterLinkValue(command, options, response.JSON200)
			return fmt.Errorf("cluster %s is linked; preview failed: %w", name, err)
		}
		if preview.JSON200 == nil {
			state, stateErr := client.GetClusterComponentsWithResponse(command.Context(), options.scope.organization, name)
			if preview.StatusCode() == http.StatusBadRequest && stateErr == nil && state.JSON200 != nil && state.JSON200.Status == "disabled" && state.JSON200.Message == "infrastructure management is disabled" {
				_, _ = fmt.Fprintln(command.ErrOrStderr(), "Cluster linked; infrastructure management is disabled, so no components were scheduled.")
				return printClusterLinkValue(command, options, response.JSON200)
			}
			_ = printClusterLinkValue(command, options, response.JSON200)
			return fmt.Errorf("cluster %s is linked; preview failed: %w", name, applicationFailure(preview.StatusCode(), preview.Body))
		}
		if err := printComponentDiff(command.ErrOrStderr(), *preview.JSON200, options.verbose); err != nil {
			return err
		}
		for _, component := range preview.JSON200.Components {
			if component.Action == "conflict" {
				_ = printClusterLinkValue(command, options, response.JSON200)
				return fmt.Errorf("cluster %s is linked; component %s conflicts with an existing installation: %s", name, component.Name, component.Reason)
			}
		}
	}
	if !options.reconcile {
		if options.output == "cli" {
			_, err := fmt.Fprintf(command.OutOrStdout(), "Cluster %s linked to %s; component setup was not scheduled.\n", name, options.scope.organization)
			return err
		}
		return printClusterLinkValue(command, options, response.JSON200)
	}
	queued, err := client.ReconcileClusterComponentsWithResponse(command.Context(), options.scope.organization, name)
	if err != nil {
		_ = printClusterLinkValue(command, options, response.JSON200)
		return fmt.Errorf("cluster %s is linked; enqueue failed: %w", name, err)
	}
	if queued.JSON200 == nil {
		_ = printClusterLinkValue(command, options, response.JSON200)
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
