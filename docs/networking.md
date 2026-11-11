# Management networking

`management.yaml` configures the management network: `network.tailscale` supplies
private administration, and `network.cloudflare` supplies management public DNS,
Cloudflare Tunnel when selected, and Let's Encrypt DNS01 certificates.

Borq uses cert-manager's built-in Cloudflare DNS01 solver. It does not deploy a
Borq DNS webhook, challenge broker, DNS01 Ingress, or private DNS01 CA issuers.
cert-manager writes public `_acme-challenge` TXT records directly through Cloudflare.
Certificate private keys remain in Kubernetes Secrets on the serving cluster.

| Component | Location | Purpose |
| --- | --- | --- |
| Tailscale operator | `borq-system` on configured physical clusters | Private administration and workload connectivity |
| ExternalDNS | `borq-mgmt` on management | Publishes accepted app and Pocket ID public DNS records |
| cert-manager | `borq-system` | Issues and renews origin certificates |
| `letsencrypt-cloudflare` ClusterIssuer | Management physical cluster | Let's Encrypt production with native Cloudflare DNS01 |
| Cloudflared, optional | Management physical cluster | Outbound connection to a Cloudflare Tunnel |

## Bootstrap

For private administration, create a management Tailscale OAuth client, enable
MagicDNS and HTTPS certificates, and configure tailnet access. Grant the OAuth
client the scopes required by the [official operator setup](https://tailscale.com/docs/kubernetes-operator/install-operator).
Keep separate remote cluster operators restricted to their own credentials.

The default management tags are `tag:borq-management-operator` for the operator
and `tag:borq-admin` for administrative HTTPS. Remote operators use
`tag:borq-operator`; workload proxies use `tag:borq`. Remote operators must not own
the management tags. Permit administrators to reach `tag:borq-admin:443`, and
permit that destination to reach itself for the connectivity check. The
administration NodePort remains available; host and network firewalls must restrict it.

Create a Cloudflare API token with **Zone Read** and **DNS Edit**, restricted to
the zone containing the configured wildcard suffix. A tunnel additionally needs
**Account / Cloudflare One Connector: cloudflared / Write** for the selected
account. cert-manager uses the native [Cloudflare DNS01 solver](https://cert-manager.io/docs/configuration/acme/dns01/cloudflare/).

```sh
export BORQ_TAILSCALE_CLIENT_ID='YOUR_MANAGEMENT_OPERATOR_CLIENT_ID'
export BORQ_TAILSCALE_CLIENT_SECRET='YOUR_MANAGEMENT_OPERATOR_CLIENT_SECRET'
export BORQ_CLOUDFLARE_API_TOKEN='YOUR_ZONE_SCOPED_TOKEN'
```

```yaml
network:
  tailscale: {}
  cloudflare:
    wildcardDomain: "*.prod-test.borq.io"
    zoneId: "YOUR_32_CHARACTER_ZONE_ID"
    acmeEmail: admin@borq.io
    tunnel:
      enabled: true
```

`wildcardDomain` accepts starred and bare suffixes equivalently; Borq stores the
bare suffix. The example authorizes names such as `id.prod-test.borq.io`. It does
not add a `management.` label. Public certificates can be requested per hostname;
a wildcard scope does not require sharing one wildcard private key among services.

```sh
borq management bootstrap \
  --kubeconfig /path/to/k3s.yaml \
  --file management.yaml
```

The public Borq server hostname is `<management-cluster-name>.<wildcardDomain>`.
The name comes from the selected kubeconfig cluster on first setup; reruns preserve
the installed name. For a cluster named `production` with the suffix above,
`production.prod-test.borq.io`
routes through its dedicated Gateway and HTTPRoute to the Borq server Service.
Direct exposure uses HTTPS and a `letsencrypt-cloudflare` Certificate; tunnel
exposure uses HTTP behind cloudflared without an origin Certificate. PocketID retains its
configured hostname and routes to its own Service; it does not share Borq's backend.
The tunnel has a single wildcard rule pointing to Traefik over cluster-local HTTP,
with the public Host preserved. Cloudflare handles public TLS and encrypts the
connection to cloudflared; the final in-cluster hop uses HTTP. Traefik makes every service
routing decision. ExternalDNS publishes the individual proxied public host records to the
tunnel target, or to the observed public Gateway address for direct exposure.
No certificate storage or distribution through OpenBao is configured.

The private hostname defaults to `borq-<management-cluster-name>`. Bootstrap
observes and verifies the assigned Tailscale HTTPS address before publishing it.
Tailscale supplies certificates for its `*.ts.net` names. Cloudflare DNS01 does
not depend on Tailscale or an exposed HTTP challenge route. Omit Tailscale when
only public provider settings are needed; omit Cloudflare for private access only.

Bootstrap stores the provider credentials in the management Kubernetes Secret
`borq-network-credentials`. It supplies the Cloudflare token separately to
cert-manager through `borq-system/borq-cloudflare-api-token`; no token is written
to Helm values, exported YAML or SQLite. The `letsencrypt-cloudflare` ClusterIssuer references
that Secret and the retained ACME account key `borq-letsencrypt-account`.

Rerun bootstrap to change public networking settings or rotate management
credentials. Omitted providers and credentials retain installed values. Ordinary
management apply accepts matching settings, but changed bootstrap settings require
bootstrap because they also change the running server's mounted configuration.
Changing the suffix or zone can leave previously published records outside the
new discovery filter; clean up the old Borq-owned DNS explicitly.

`network.tailscale: {}` currently means enable defaults. It does not clear values
or uninstall Tailscale. This differs from addon maps whose empty object clears
values. Explicit network enablement is a future schema improvement; `enabled`
is not currently accepted under `tailscale`. The old `brokerTag` and
`webhookImage` fields are accepted only for upgrade compatibility and ignored.

When replacing an older test installation, Borq retires only verified Borq-owned
DNS01 webhook resources after the native issuer is ready. An unused old
`letsencrypt` or `cloudflare` webhook issuer is removed; an alias still referenced by Certificates switches
to the native solver so its references can continue to renew. Active old webhook
Challenges must be retried with the native issuer before retiring that webhook.
Other cert-manager webhooks and their TLS configuration are unaffected.

## Exposure modes

Choose `network.cloudflare.exposure: tunnel` or `direct`. The choice controls
management services, including Borq and PocketID. It does not redirect another
physical cluster through the management tunnel. Both paths use ExternalDNS and
Traefik; neither configures a zone-wide Cloudflare for SaaS fallback.

| Exposure | Public DNS target | Network path |
| --- | --- | --- |
| `tunnel` | Proxied CNAME to `<tunnel-id>.cfargotunnel.com` | Cloudflare tunnel to Traefik |
| `direct` | Proxied A/AAAA/CNAME to a public Gateway address or `directTarget` | Cloudflare proxy to public Traefik ingress |

For a tunnel, use the existing provider settings with:

```yaml
network:
  cloudflare:
    exposure: tunnel
    wildcardDomain: "*.prod-test.borq.io"
    zoneId: "YOUR_32_CHARACTER_ZONE_ID"
    acmeEmail: admin@borq.io
```

This enables the pinned connector. Its wildcard route points only to Traefik,
which selects the backend through HTTP Gateway listeners. Borq and PocketID keep
their public HTTPS URLs, but request no origin Certificate in tunnel mode.
A pending or unavailable tunnel preserves the previous DNS record and never
falls back to a load-balancer address. A management tunnel error does not block
publication for an unrelated deployment cluster.

For direct exposure, use:

```yaml
network:
  cloudflare:
    exposure: direct
    directTarget: public-load-balancer.example.net
    wildcardDomain: "*.prod-test.borq.io"
    zoneId: "YOUR_32_CHARACTER_ZONE_ID"
    acmeEmail: admin@borq.io
```

`directTarget` accepts one public IPv4/IPv6 address or load-balancer hostname.
Omit it when the Gateway controller reports the correct public address. It does
not provision a load balancer or configure NAT: public port 443 must already
reach Traefik. Private addresses, internal/tailnet hostnames and tunnel targets
are rejected. Direct targets cannot be combined with tunnel exposure.

Existing `tunnel.enabled` files remain readable and infer the same exposure mode.
An explicit mode that contradicts `tunnel.enabled` is rejected. Switching to
direct keeps an existing connector running until a public Gateway address or
explicit direct target is available. Public DNS caches can still outlive a mode
change; configure and verify the new destination before changing modes.

Direct exposure retains cert-manager's native Let's Encrypt/Cloudflare DNS01
origin certificates. Tunnel exposure retires the obsolete Borq-owned origin
Certificate resources. The native issuer remains available for independent direct
services, but tunnel setup does not wait for ACME readiness. Neither mode changes
Cloudflare's browser-facing certificate coverage.
These exposure fields require a CLI and server containing this change; rc6
continues to accept its existing `tunnel.enabled` configuration.

## Workload spaces

The management provider credentials are not inherited by separate application
clusters. Existing space-level `certificateIssuer` selection determines which
ClusterIssuer its public HTTPRoute Certificates reference. That issuer must exist
on the space's cluster. A converged management/application cluster can select the
management `letsencrypt-cloudflare` issuer. A separate cluster can use a native Cloudflare
ClusterIssuer with its own zone-scoped Secret; it does not use a Borq DNS broker.

Independent per-space provider, zone and credential-reference provisioning is
planned, not implemented by `network.cloudflare`. A single native issuer can use
multiple DNS solver selectors for multiple zones; a separate ClusterIssuer per
zone is optional. Solver selectors choose a solver but do not replace Kubernetes
certificate-request authorization or Cloudflare credential restrictions.

Separate application clusters can still configure their own Tailscale OAuth pair:

```yaml
name: apps-production
organization: example
kubeconfigPath: ./apps-production.kubeconfig
tailscale:
  clientId: YOUR_RESTRICTED_CLUSTER_CLIENT_ID
  clientSecret: YOUR_RESTRICTED_CLUSTER_CLIENT_SECRET
```

Use `borq cluster link --file cluster.yaml`. Borq stores credentials privately and
exports references only. Omission preserves installed credentials. A converged
physical cluster reuses the management operator without another credential pair.

## Routing and public TLS

Public applications use dedicated Traefik HTTPRoute and Gateway resources with
explicit cert-manager Certificates. Management public DNS still observes accepted
routes; pending routes preserve the last known DNS rather than removing it.
Application hostnames inside the management suffix retain their assigned
`<cluster-id>.<wildcardDomain>` subtree. Managed identity names use one label
directly below the suffix. Existing names are never silently renamed.

With direct exposure, supply a reachable public load balancer or static public IP,
route port 443 to Traefik. ExternalDNS requests proxied Cloudflare records for
the managed public hostnames. DNS01 proves domain control; it does not expose a
private cluster to the internet.

Cloudflare proxying adds a separate browser-facing TLS connection. The Kubernetes
Certificate protects the origin; Cloudflare presents an edge certificate to the
browser. For a full `borq.io` zone, Universal SSL covers `borq.io` and `*.borq.io`,
not `id.prod-test.borq.io`. Proxying that deeper name requires separate
edge certificate coverage. Cloudflare documents different coverage for full DNS
and CNAME/partial zone setups; verify the actual zone configuration. [Cloudflare coverage limits](https://developers.cloudflare.com/ssl/edge-certificates/universal-ssl/limitations/).

The native cert-manager solver creates origin certificates and DNS challenge
records; it does not subscribe to paid Cloudflare services, upload private keys,
or configure Cloudflare edge certificates.

## Verification

`make test-network` verifies configuration, provider protocol, native issuer
selection and retirement ownership guards. `make test-network-bootstrap-integration`
checks actual Kubernetes credential staging and rotation. DNS inventory integration
uses explicit Gateway and HTTPRoute status inputs; it does not claim controller
success. Verify real DNS01 issuance, public TLS, and renewal against the intended
Cloudflare zone before relying on a deployment.
