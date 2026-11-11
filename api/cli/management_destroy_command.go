package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/stefanmcshane/borq/api/appapi"
	"github.com/stefanmcshane/borq/business/cli/management"
	"github.com/stefanmcshane/borq/business/server/platform"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

type managementDestroyOptions struct {
	scope   applicationScope
	input   platform.ManagementDestroyInput
	confirm bool
}

func newManagementDestroyCommand() *cobra.Command {
	options := managementDestroyOptions{input: platform.ManagementDestroyInput{Namespace: "borq-mgmt", Timeout: 10 * time.Minute}}
	command := &cobra.Command{Use: "destroy", Short: "Ask the server to detach management exposure, then remove Borq; retain data.", Long: "Print a read-only plan from the management server using its private Kubernetes Service proxy and your saved administrator login. Add --confirm to queue durable server-owned cleanup. After validating its completed Kubernetes receipt, the CLI removes only Borq's final core resources. Retains data and shared infrastructure.", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error { return executeManagementDestroy(command, options) },
	}
	addApplicationServerFlag(command, &options.scope)
	command.Flags().StringVar(&options.input.KubeconfigPath, "kubeconfig", "", "Explicit kubeconfig for the management installation and private Borq connection.")
	command.Flags().StringVar(&options.input.Namespace, "namespace", "borq-mgmt", "Management namespace to detach.")
	command.Flags().BoolVar(&options.confirm, "confirm", false, "Execute the server-owned teardown; omitted prints a read-only plan.")
	command.Flags().DurationVar(&options.input.Timeout, "timeout", 10*time.Minute, "Maximum local wait; accepted server cleanup continues after timeout.")
	_ = command.MarkFlagRequired("kubeconfig")
	return command
}

func executeManagementDestroy(command *cobra.Command, options managementDestroyOptions) error {
	if strings.TrimSpace(options.input.KubeconfigPath) == "" {
		return errors.New("management teardown requires an explicit --kubeconfig")
	}
	if options.input.Timeout <= 0 {
		return errors.New("--timeout must be greater than zero")
	}
	origin, err := canonicalServerOrigin(options.scope.server)
	if err != nil {
		return err
	}
	credential, err := credentialPath(origin)
	if err != nil {
		return err
	}
	token, err := readPrivateToken(credential)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("management teardown requires a saved administrator login; run borq login --api-url %s --token-file PATH", origin)
	}
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(command.Context(), options.input.Timeout)
	defer cancel()

	// A completed, immutable server receipt permits recovery after Borq itself
	// has stopped. Missing or stale receipts require the current server API.
	receipt, err := platform.ReadManagementDestroyReceipt(ctx, options.input)
	if err == nil {
		return finishManagementDestroy(command, ctx, options, receipt)
	}
	if !apierrors.IsNotFound(err) && !errors.Is(err, platform.ErrManagementDestroyReceiptStale) {
		return fmt.Errorf("read server teardown receipt: %w", err)
	}
	access, err := management.ConnectPrivateServer(management.PrivateServerInput{KubeconfigPath: options.input.KubeconfigPath, Namespace: options.input.Namespace, AdminToken: token})
	if err != nil {
		return err
	}
	client, err := appapi.NewClientWithResponses(access.URL, appapi.WithHTTPClient(access.HttpClient))
	if err != nil {
		return err
	}
	if !options.confirm {
		response, err := client.PlanManagementDestroyWithResponse(ctx)
		if err != nil {
			return fmt.Errorf("read private server teardown plan: %w", err)
		}
		if response.JSON200 == nil {
			return applicationFailure(response.StatusCode(), response.Body)
		}
		return printApplicationValue(command.OutOrStdout(), *response.JSON200)
	}
	response, err := client.DestroyManagementWithResponse(ctx)
	if err != nil {
		return fmt.Errorf("submit private server teardown: %w", err)
	}
	if response.JSON202 == nil {
		return applicationFailure(response.StatusCode(), response.Body)
	}
	operation := response.JSON202.ID
	if operation == "" {
		return errors.New("management server returned no teardown operation ID")
	}
	if _, err := fmt.Fprintf(command.ErrOrStderr(), "Management teardown operation %s; server work continues if this command stops.\n", operation); err != nil {
		return err
	}
	if err := waitManagementDestroy(ctx, client, operation); err != nil {
		return err
	}
	options.input.OperationID = operation
	receipt, err = platform.ReadManagementDestroyReceipt(ctx, options.input)
	if err != nil {
		return fmt.Errorf("verify completed server teardown receipt before removing Borq: %w", err)
	}
	return finishManagementDestroy(command, ctx, options, receipt)
}

func waitManagementDestroy(ctx context.Context, client *appapi.ClientWithResponses, operation string) error {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		response, err := client.GetManagementDestroyWithResponse(ctx, operation)
		if err != nil {
			return fmt.Errorf("read private server teardown progress; retry this command to resume: %w", err)
		}
		if response.JSON200 == nil {
			return applicationFailure(response.StatusCode(), response.Body)
		}
		status := response.JSON200.Operation
		if status.ID != operation {
			return errors.New("management teardown status returned a different operation")
		}
		switch status.Status {
		case appapi.Succeeded:
			return nil
		case appapi.Failed, appapi.Canceled:
			return fmt.Errorf("server teardown %s: %s; resolve the failure and retry", status.Status, status.Message)
		case appapi.Queued, appapi.Running:
		default:
			return fmt.Errorf("management server returned unknown teardown status %q", status.Status)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("local teardown wait ended; server work continues, retry this command to resume: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func finishManagementDestroy(command *cobra.Command, ctx context.Context, options managementDestroyOptions, receipt platform.ManagementDestroyReceipt) error {
	if !options.confirm {
		if _, err := fmt.Fprintln(command.ErrOrStderr(), "Server cleanup is complete. Add --confirm to remove Borq's remaining core resources."); err != nil {
			return err
		}
		return printApplicationValue(command.OutOrStdout(), receipt.Plan)
	}
	options.input.OperationID = receipt.OperationID
	result, err := platform.FinalizeManagementServer(ctx, options.input, receipt)
	if err != nil {
		return fmt.Errorf("finalize Borq after completed server cleanup: %w", err)
	}
	return printApplicationValue(command.OutOrStdout(), result)
}
