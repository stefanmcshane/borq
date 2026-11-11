package platform

import (
	"context"
	"fmt"
	"strings"

	"github.com/stefanmcshane/borq/foundations/cloudflare"
	"github.com/stefanmcshane/borq/foundations/kubernetes"
	"github.com/stefanmcshane/borq/foundations/logger"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

func CheckManagementDnsOwnership(ctx context.Context, input ManagementInput) error {
	if input.Settings.Network.Cloudflare == nil {
		return nil
	}
	settings, err := normalizeSettings(input.Settings)
	if err != nil {
		return err
	}
	connection, err := kubernetes.NewConnection(kubernetes.Config{KubeconfigPath: input.KubeconfigPath}, logger.Logger{})
	if err != nil {
		return err
	}
	client, err := networkClient(connection)
	if err != nil {
		return err
	}
	var endpoints unstructured.UnstructuredList
	endpoints.SetGroupVersionKind(schema.GroupVersionKind{Group: "externaldns.k8s.io", Version: "v1alpha1", Kind: "DNSEndpointList"})
	if err := client.List(ctx, &endpoints, ctrlclient.InNamespace(settings.Namespace), ctrlclient.MatchingLabels{"app.kubernetes.io/managed-by": "borq", "borq.run/network-dns": "true"}); err != nil {
		return fmt.Errorf("read managed DNS endpoints for ownership check: %w", err)
	}
	var records []cloudflare.DnsOwnershipRecord
	for _, endpoint := range endpoints.Items {
		entries, _, err := unstructured.NestedSlice(endpoint.Object, "spec", "endpoints")
		if err != nil {
			return fmt.Errorf("read DNS endpoint %s for ownership check: %w", endpoint.GetName(), err)
		}
		for _, entry := range entries {
			fields, ok := entry.(map[string]any)
			if !ok {
				return fmt.Errorf("DNS endpoint %s has an invalid record", endpoint.GetName())
			}
			hostname, _, err := unstructured.NestedString(fields, "dnsName")
			if err != nil {
				return fmt.Errorf("read DNS endpoint %s hostname: %w", endpoint.GetName(), err)
			}
			kind, _, err := unstructured.NestedString(fields, "recordType")
			if err != nil {
				return fmt.Errorf("read DNS endpoint %s record type: %w", endpoint.GetName(), err)
			}
			if hostname == settings.Network.Cloudflare.WildcardDomain || strings.HasSuffix(hostname, "."+settings.Network.Cloudflare.WildcardDomain) {
				records = append(records, cloudflare.DnsOwnershipRecord{Hostname: hostname, RecordType: kind})
			}
		}
	}
	if len(records) == 0 {
		return nil
	}
	credentials, err := readNetworkBootstrapCredentials(ctx, client, settings)
	if err != nil {
		return err
	}
	owner, err := networkOwnerID(ctx, connection)
	if err != nil {
		return err
	}
	api, err := cloudflare.New(cloudflare.Config{ZoneID: settings.Network.Cloudflare.ZoneID, ApiToken: string(credentials.Data["cloudflareApiToken"])})
	if err != nil {
		return fmt.Errorf("configure Cloudflare DNS ownership check")
	}
	return api.CheckExternalDnsOwnership(ctx, cloudflare.DnsOwnershipInput{OwnerID: owner, Records: records})
}
