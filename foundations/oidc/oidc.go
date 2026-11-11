// Package oidc implements discovery and the OpenID Connect authorization code flow.
package oidc

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	provider "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// Config identifies a confidential web client registered with one issuer.
type Config struct {
	// Issuer is the exact provider identifier, including any tenant path.
	Issuer string
	// ClientID identifies this Borq organization at the provider.
	ClientID string
	// ClientSecret authenticates the code exchange and must never reach a browser.
	ClientSecret string
	// RedirectURL is the exact callback registered with the provider.
	RedirectURL string
}

// Client retains discovered endpoints for one authorization attempt.
type Client struct {
	configuration oauth2.Config
	// The provider library shares its signing-key cache through a pointer.
	verifier               *provider.IDTokenVerifier
	issuer                 string
	responseIssuerRequired bool
	logoutEndpoint         url.URL
	logoutReturnURL        string
}

// Identity contains verified claims, never access, refresh, or ID tokens.
type Identity struct {
	// Issuer and Subject together identify the account; email is not an identity key.
	Issuer, Subject string
	// Name and Email are optional display claims supplied by the provider.
	Name, Email string
	// EmailVerified reports the provider's assertion without inferring membership.
	EmailVerified bool
	// ExpiresAt limits any session created from this authentication.
	ExpiresAt time.Time
}

// Authentication contains verified identity and transient provider session details.
type Authentication struct {
	// Identity contains only the verified claims safe for application policy.
	Identity Identity
	// LogoutURL contains the verified ID token hint and must stay in private session memory.
	LogoutURL string `json:"-"`
}

// ValidateURL requires TLS except for localhost and literal loopback addresses used in local development.
// Credentials, queries and fragments are not valid issuer or callback configuration.
func ValidateURL(address string) error {
	parsed, err := url.Parse(address)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.Opaque != "" || strings.ContainsAny(address, "?#\r\n\t \\") {
		return errors.New("enter an absolute HTTPS URL without credentials, a query, or a fragment")
	}
	if parsed.Scheme == "https" {
		return nil
	}
	if parsed.Scheme == "http" && (parsed.Hostname() == "localhost" || net.ParseIP(parsed.Hostname()).IsLoopback()) {
		return nil
	}
	return errors.New("HTTPS is required; HTTP is allowed only on localhost or a literal loopback address")
}

func clientContext(ctx context.Context) context.Context {
	// Never forward a code or client secret through an endpoint redirect.
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("OIDC endpoint redirects are not allowed")
	}}
	return provider.ClientContext(ctx, client)
}

// New discovers a provider and checks its endpoints before using client credentials.
func New(ctx context.Context, config Config) (Client, error) {
	if err := ValidateURL(config.Issuer); err != nil {
		return Client{}, err
	}
	if err := ValidateURL(config.RedirectURL); err != nil {
		return Client{}, err
	}
	callback, err := url.Parse(config.RedirectURL)
	if err != nil {
		return Client{}, errors.New("OIDC callback URL is invalid")
	}
	if strings.TrimSpace(config.ClientID) == "" || config.ClientSecret == "" {
		return Client{}, errors.New("client ID and client secret are required")
	}
	ctx = clientContext(ctx)
	discovered, err := provider.NewProvider(ctx, config.Issuer)
	if err != nil {
		return Client{}, errors.New("OIDC discovery failed; check the issuer URL and its TLS certificate")
	}
	var metadata struct {
		Issuer                  string `json:"issuer"`
		Keys                    string `json:"jwks_uri"`
		ResponseIssuerSupported bool   `json:"authorization_response_iss_parameter_supported"`
		EndSession              string `json:"end_session_endpoint"`
	}
	if err := discovered.Claims(&metadata); err != nil || metadata.Issuer != config.Issuer {
		return Client{}, errors.New("OIDC discovery did not return the configured issuer")
	}
	endpoint := discovered.Endpoint()
	addresses := []string{endpoint.AuthURL, endpoint.TokenURL, metadata.Keys}
	if metadata.EndSession != "" {
		addresses = append(addresses, metadata.EndSession)
	}
	var logoutEndpoint url.URL
	for _, address := range addresses {
		endpointURL, err := url.Parse(address)
		if err != nil || strings.ContainsAny(address, "#\r\n\t \\") {
			return Client{}, errors.New("OIDC discovery returned an invalid endpoint")
		}
		// Provider endpoints may have fixed query parameters; issuer identifiers may not.
		if _, err := url.ParseQuery(endpointURL.RawQuery); err != nil {
			return Client{}, errors.New("OIDC discovery returned an invalid endpoint")
		}
		addressWithoutQuery := *endpointURL
		addressWithoutQuery.RawQuery, addressWithoutQuery.ForceQuery = "", false
		if err := ValidateURL(addressWithoutQuery.String()); err != nil {
			return Client{}, errors.New("OIDC discovery returned an invalid endpoint")
		}
		if strings.HasPrefix(config.Issuer, "https:") && !strings.HasPrefix(address, "https:") {
			return Client{}, errors.New("OIDC discovery returned an endpoint without HTTPS")
		}
		if address == metadata.EndSession {
			logoutEndpoint = *endpointURL
		}
	}
	return Client{
		configuration: oauth2.Config{ClientID: config.ClientID, ClientSecret: config.ClientSecret, Endpoint: endpoint, RedirectURL: config.RedirectURL, Scopes: []string{provider.ScopeOpenID, "profile", "email"}},
		verifier:      discovered.Verifier(&provider.Config{ClientID: config.ClientID}), issuer: config.Issuer,
		responseIssuerRequired: metadata.ResponseIssuerSupported,
		logoutEndpoint:         logoutEndpoint,
		logoutReturnURL:        callback.ResolveReference(&url.URL{Path: "/login"}).String(),
	}, nil
}

// CheckResponseIssuer validates RFC 9207 before sending a code to the token endpoint.
// Callers must also use a distinct callback per issuer when the provider omits this parameter.
func (client Client) CheckResponseIssuer(issuer string) error {
	if (issuer == "" && client.responseIssuerRequired) || (issuer != "" && issuer != client.issuer) {
		return errors.New("OIDC authorization response does not identify the expected issuer")
	}
	return nil
}

// AuthorizationURL binds an authorization code to state, nonce, and an S256 PKCE challenge.
func (client Client) AuthorizationURL(state, nonce, verifier string) string {
	return client.configuration.AuthCodeURL(state, provider.Nonce(nonce), oauth2.S256ChallengeOption(verifier))
}

// Exchange validates the signed identity, audience, expiry and nonce after a PKCE code exchange.
// Provider responses are deliberately excluded from errors because they can contain tokens.
func (client Client) Exchange(ctx context.Context, code, nonce, verifier string) (Authentication, error) {
	if code == "" || nonce == "" || verifier == "" {
		return Authentication{}, errors.New("incomplete OIDC authorization response")
	}
	ctx = clientContext(ctx)
	token, err := client.configuration.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return Authentication{}, errors.New("OIDC code exchange failed")
	}
	raw, ok := token.Extra("id_token").(string)
	if !ok || raw == "" {
		return Authentication{}, errors.New("OIDC provider did not return an ID token")
	}
	verified, err := client.verifier.Verify(ctx, raw)
	if err != nil {
		return Authentication{}, errors.New("OIDC identity validation failed")
	}
	if verified.Subject == "" || subtle.ConstantTimeCompare([]byte(verified.Nonce), []byte(nonce)) != 1 {
		return Authentication{}, errors.New("OIDC identity does not match this sign-in attempt")
	}
	var claims struct {
		Name  string `json:"name"`
		Email string `json:"email"`
		// Presence distinguishes an explicit denial from a missing standard claim.
		EmailVerified       *bool           `json:"email_verified"`
		EmailDomainVerified json.RawMessage `json:"xms_edov"`
		TenantID            json.RawMessage `json:"tid"`
		AuthorizedParty     string          `json:"azp"`
	}
	if err := verified.Claims(&claims); err != nil {
		return Authentication{}, errors.New("OIDC identity claims are invalid")
	}
	if (len(verified.Audience) > 1 && claims.AuthorizedParty == "") || (claims.AuthorizedParty != "" && claims.AuthorizedParty != client.configuration.ClientID) {
		return Authentication{}, errors.New("OIDC identity was issued to another client")
	}
	emailVerified := false
	if claims.EmailVerified != nil {
		emailVerified = *claims.EmailVerified
	} else {
		emailVerified = microsoftVerifiedEmailDomain(client.issuer, claims.Email, claims.EmailDomainVerified, claims.TenantID)
	}
	identity := Identity{Issuer: client.issuer, Subject: verified.Subject, Name: claims.Name, Email: claims.Email, EmailVerified: emailVerified, ExpiresAt: verified.Expiry}
	authentication := Authentication{Identity: identity}
	if client.logoutEndpoint.Host != "" {
		endpoint := client.logoutEndpoint
		query := endpoint.Query()
		query.Set("client_id", client.configuration.ClientID)
		query.Set("id_token_hint", raw)
		query.Set("post_logout_redirect_uri", client.logoutReturnURL)
		endpoint.RawQuery = query.Encode()
		authentication.LogoutURL = endpoint.String()
	}
	return authentication, nil
}
