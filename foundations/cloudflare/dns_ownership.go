package cloudflare

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

type DnsOwnershipInput struct {
	OwnerID string
	Records []DnsOwnershipRecord
}

type DnsOwnershipRecord struct {
	Hostname   string
	RecordType string
}

// CheckExternalDnsOwnership observes Borq's TXT registry without adopting records
// from another installation. Missing records are normal during initial publication.
func (client Client) CheckExternalDnsOwnership(ctx context.Context, input DnsOwnershipInput) error {
	if input.OwnerID == "" {
		return errors.New("ExternalDNS ownership check requires an installation owner ID")
	}
	names := map[string]string{}
	for _, record := range input.Records {
		hostname := canonicalName(record.Hostname)
		if len(validation.IsDNS1123Subdomain(strings.TrimPrefix(hostname, "*."))) != 0 {
			return errors.New("ExternalDNS ownership check requires valid DNS hostnames")
		}
		kind := strings.ToLower(record.RecordType)
		if kind != "a" && kind != "aaaa" && kind != "cname" {
			return errors.New("ExternalDNS ownership check requires A, AAAA or CNAME records")
		}
		// These names match the pinned ExternalDNS registry's type prefix and
		// wildcard replacement in platform/values/external-dns.yaml.
		name := strings.Replace(hostname, "*.", "_wildcard.", 1)
		names["_borq."+kind+"-"+name] = hostname
	}
	if len(names) == 0 {
		return nil
	}
	records, err := client.list(ctx, url.Values{"name.startswith": {"_borq."}})
	if err != nil {
		return fmt.Errorf("read ExternalDNS ownership records: %w", err)
	}
	var failures []error
	for _, record := range records {
		hostname, claimed := names[canonicalName(record.Name)]
		if !claimed || record.Type != "TXT" {
			continue
		}
		content := record.Content
		if strings.HasPrefix(content, "\"") {
			content, err = strconv.Unquote(content)
			if err != nil {
				failures = append(failures, fmt.Errorf("DNS hostname %s has an invalid ExternalDNS ownership record", hostname))
				continue
			}
		}
		var heritage, owner string
		for _, label := range strings.Split(content, ",") {
			key, value, _ := strings.Cut(label, "=")
			switch key {
			case "heritage":
				heritage = value
			case "external-dns/owner":
				owner = value
			}
		}
		if heritage == "external-dns" && owner != input.OwnerID {
			failures = append(failures, fmt.Errorf("DNS hostname %s has ExternalDNS owner %q; expected %q; DNS updates are blocked until ownership is explicitly transferred to this installation", hostname, owner, input.OwnerID))
		}
	}
	return errors.Join(failures...)
}
