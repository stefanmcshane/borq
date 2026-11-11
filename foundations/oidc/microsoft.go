package oidc

import (
	"encoding/json"
	"net/url"
	"strings"

	"github.com/google/uuid"
)

func microsoftVerifiedEmailDomain(issuer, email string, domainClaim, tenantClaim json.RawMessage) bool {
	if email == "" {
		return false
	}
	address, err := url.Parse(issuer)
	if err != nil || address.Scheme != "https" || address.Host != "login.microsoftonline.com" || address.User != nil || address.RawQuery != "" || address.Fragment != "" {
		return false
	}
	parts := strings.Split(address.Path, "/")
	if len(parts) != 3 || parts[0] != "" || parts[2] != "v2.0" || len(parts[1]) != 36 {
		return false
	}
	tenant, err := uuid.Parse(parts[1])
	if err != nil || tenant == uuid.Nil {
		return false
	}
	var domainVerified bool
	var tokenTenant string
	if json.Unmarshal(domainClaim, &domainVerified) != nil || !domainVerified || json.Unmarshal(tenantClaim, &tokenTenant) != nil {
		return false
	}
	// Microsoft vouches for the email's tenant domain. Accept this assertion only
	// from that exact tenant's verified issuer, never from another provider's claim.
	return strings.EqualFold(tokenTenant, tenant.String())
}
