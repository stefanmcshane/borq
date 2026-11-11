package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/spf13/cobra"
	"github.com/stefanmcshane/borq/api/appapi"
)

type pocketIdOptions struct {
	scope                                                                                 applicationScope
	action, name, file                                                                    string
	issuerUrl, adminUsername, adminEmail, storage, storageClass, image, certificateIssuer string
	wait                                                                                  bool
	timeout                                                                               time.Duration
}

func newPocketIdCommand() *cobra.Command {
	group := &cobra.Command{Use: "pocketid", Short: "Host Pocket ID with persistent SQLite storage.", Args: cobra.NoArgs}
	for _, action := range []string{"create", "apply", "list", "get", "delete", "enroll", "operation"} {
		group.AddCommand(newPocketIdAction(action))
	}
	return group
}

func newPocketIdAction(action string) *cobra.Command {
	options := pocketIdOptions{action: action}
	use, args := action+" NAME", cobra.ExactArgs(1)
	if action == "list" {
		use, args = action, cobra.NoArgs
	}
	if action == "apply" {
		use, args = "apply [NAME]", cobra.MaximumNArgs(1)
	}
	command := &cobra.Command{Use: use, Short: "Pocket ID " + action + ".", Args: args, RunE: func(command *cobra.Command, arguments []string) error {
		if len(arguments) > 0 {
			options.name = arguments[0]
		}
		return executePocketId(command, options)
	}}
	addApplicationServerFlag(command, &options.scope)
	if action != "operation" {
		command.Flags().StringVar(&options.scope.organization, "organization", "", "Organization owning the space.")
		command.Flags().StringVar(&options.scope.space, "space", "", "Space owning Pocket ID.")
	}
	if action == "create" || action == "apply" {
		command.Flags().StringVarP(&options.file, "file", "f", "", "PocketIdInput YAML; apply without NAME accepts a space configuration.")
		command.Flags().StringVar(&options.issuerUrl, "issuer-url", "", "Stable public HTTPS URL for Pocket ID.")
		command.Flags().StringVar(&options.adminUsername, "admin-username", "", "Initial administrator username.")
		command.Flags().StringVar(&options.adminEmail, "admin-email", "", "Initial administrator email address.")
		command.Flags().StringVar(&options.storage, "storage", "", "Persistent SQLite storage capacity.")
		command.Flags().StringVar(&options.storageClass, "storage-class", "", "Storage class providing ReadWriteOnce block storage.")
		command.Flags().StringVar(&options.image, "image", "", "Pinned Pocket ID image; omitted uses the tested default.")
		command.Flags().StringVar(&options.certificateIssuer, "certificate-issuer", "", "Certificate issuer; omitted uses the space issuer.")
	}
	if action == "create" || action == "apply" || action == "delete" {
		command.Flags().BoolVar(&options.wait, "wait", true, "Wait for the accepted operation to complete.")
		command.Flags().DurationVar(&options.timeout, "timeout", 15*time.Minute, "Maximum local wait; server work continues after timeout.")
	}
	return command
}

func executePocketId(command *cobra.Command, options pocketIdOptions) error {
	if options.action != "operation" {
		if err := validateApplicationScope(options.scope); err != nil {
			return err
		}
	}
	if options.wait && options.timeout <= 0 {
		return errors.New("--timeout must be greater than zero")
	}
	client, err := applicationClient(options.scope.server)
	if err != nil {
		return err
	}
	ctx := command.Context()
	organization, space, name := options.scope.organization, options.scope.space, options.name
	switch options.action {
	case "list":
		response, err := client.ListPocketID(ctx, organization, space)
		value, err := readManagedResponse[[]appapi.PocketID](response, err)
		if err != nil {
			return err
		}
		return printApplicationValue(command.OutOrStdout(), value)
	case "get":
		response, err := client.GetPocketID(ctx, organization, space, name)
		value, err := readManagedResponse[appapi.PocketID](response, err)
		if err != nil {
			return err
		}
		return printApplicationValue(command.OutOrStdout(), value)
	case "operation":
		response, err := client.GetPocketIDOperation(ctx, name)
		value, err := readManagedResponse[appapi.Operation](response, err)
		if err != nil {
			return err
		}
		return printApplicationValue(command.OutOrStdout(), value)
	case "enroll":
		response, err := client.CreatePocketIDEnrollment(ctx, organization, space, name)
		value, err := readManagedResponse[appapi.PocketIDEnrollment](response, err)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(command.OutOrStdout(), value.URL)
		return err
	case "apply":
		if name == "" {
			if options.file == "" {
				return errors.New("provide NAME or --file")
			}
			var body appapi.SpaceConfiguration
			if err := readManagedDocument(options.file, "SpaceConfiguration", &body); err != nil {
				return err
			}
			response, err := client.ApplySpaceConfiguration(ctx, organization, space, body)
			operation, err := readManagedResponse[appapi.Operation](response, err)
			if err != nil {
				return err
			}
			return finishManagedOperation(command, client, operation, options.wait, options.timeout, true)
		}
	}
	var response *http.Response
	switch options.action {
	case "create", "apply":
		body := appapi.PocketIDInput{}
		if options.file != "" {
			if err := readManagedDocument(options.file, "PocketIdInput", &body); err != nil {
				return err
			}
		}
		// Generated optional fields use pointers so omitted flags preserve file values.
		for _, field := range []struct {
			flag   string
			value  string
			target **string
		}{
			{"issuer-url", options.issuerUrl, &body.IssuerURL},
			{"admin-username", options.adminUsername, &body.AdminUsername},
			{"admin-email", options.adminEmail, &body.AdminEmail},
			{"storage", options.storage, &body.Storage},
			{"storage-class", options.storageClass, &body.StorageClass},
			{"image", options.image, &body.Image},
			{"certificate-issuer", options.certificateIssuer, &body.CertificateIssuer},
		} {
			if command.Flags().Changed(field.flag) {
				*field.target = &field.value
			}
		}
		apply := options.action == "apply"
		response, err = client.PutPocketID(ctx, organization, space, name, &appapi.PutPocketIDParams{Apply: &apply}, body)
	case "delete":
		response, err = client.DeletePocketID(ctx, organization, space, name)
	default:
		return errors.New("unsupported managed Pocket ID action")
	}
	value, err := readManagedResponse[appapi.PocketID](response, err)
	if err != nil {
		return err
	}
	if !options.wait {
		return printApplicationValue(command.OutOrStdout(), value)
	}
	return finishPocketIdOperation(command, client, value.Operation, options.timeout)
}

// Cobra and the generated client require pointers for output and HTTP connection state.
func finishPocketIdOperation(command *cobra.Command, client *appapi.ClientWithResponses, identifier string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(command.Context(), timeout)
	defer cancel()
	for {
		response, err := client.GetPocketIDOperation(ctx, identifier)
		operation, err := readManagedResponse[appapi.Operation](response, err)
		if err != nil {
			return err
		}
		if operation.Status == appapi.Succeeded {
			return printApplicationValue(command.OutOrStdout(), operation)
		}
		if operation.Status == appapi.Failed || operation.Status == appapi.Canceled {
			return fmt.Errorf("operation %s %s: %s", operation.ID, operation.Status, operation.Message)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}
