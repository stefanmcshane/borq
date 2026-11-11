# K3s platform managed by Flux

The bootstrap script installs K3s and Flux. Flux then owns the rest of the
platform.

The layout has three reconciliation phases because custom resources cannot be
created reliably before their controllers and secrets exist:

```text
gitops/k3s/
├── bootstrap.sh
├── flux.yaml
├── controllers/resources.yaml
├── secrets/resources.yaml
└── services/resources.yaml
```

Each phase has a tiny `kustomization.yaml` beside its single resource file.

## Bootstrap

On a fresh Debian or Ubuntu VPS, run the bootstrap script as root and give it
the GitHub repository containing this directory:

```sh
sudo ./gitops/k3s/bootstrap.sh stefanmcshane/borq
```

It prompts for a read-only GitHub token and a config-scoped Doppler service
token, then installs pinned K3s and Flux versions, creates the Git source and
root Kustomization, and waits for reconciliation. Pass the branch as the second
argument when it is not `main`. Only the Flux source, Kustomize, and Helm
controllers are installed; this stack does not need the notification or image
automation controllers.

For unattended provisioning, use
`sudo GITHUB_TOKEN=... DOPPLER_TOKEN=... ./gitops/k3s/bootstrap.sh owner/repo`.
`K3S_VERSION` and `FLUX_VERSION` may be set when deliberately upgrading the
pinned versions.

The script disables K3s ServiceLB so Traefik is reachable through Cloudflare
Tunnel without also binding public host ports 80 and 443. The Doppler token is
the only platform Secret not managed by ESO because ESO needs it to reach
Doppler in the first place.

## Doppler keys

`secrets/resources.yaml` is the complete declaration of the Kubernetes Secrets
pulled by ESO. Its Doppler config must contain:

- `K3S_CLUSTER_NAME`
- `CLOUDFLARE_ACCOUNT_ID`
- `CLOUDFLARE_DOMAIN`
- `CLOUDFLARE_TUNNEL_NAME`
- `CLOUDFLARE_API_TOKEN`
- `GRAFANA_CLOUD_METRICS_URL`
- `GRAFANA_CLOUD_METRICS_USERNAME`
- `GRAFANA_CLOUD_LOGS_URL`
- `GRAFANA_CLOUD_LOGS_USERNAME`
- `GRAFANA_CLOUD_ACCESS_POLICY_TOKEN`
- `TAILSCALE_OAUTH_CLIENT_ID`
- `TAILSCALE_OAUTH_CLIENT_SECRET`
- `R2_BUCKET`
- `R2_ACCESS_KEY_ID`
- `R2_SECRET_ACCESS_KEY`

Flux substitutes the non-secret deployment values from the ESO-created
`platform-config` Secret. There are no values to replace in the manifests.
Use an R2 token restricted to the backup bucket. The Grafana Cloud token needs
`metrics:write` and `logs:write`.

## What Flux installs

- cert-manager and External Secrets Operator
- the community Cloudflare Operator and a wildcard tunnel route to K3s Traefik
- the Tailscale Kubernetes Operator
- Grafana Alloy and kube-state-metrics, sending logs and metrics to Grafana Cloud
- CloudNativePG, the CNPG-I Barman Cloud plugin, `borq-postgres`, continuous WAL
  archiving, and daily R2 backups with a 30-day retention policy

Alloy discovers ready Pods with a port named `metrics`, so there is no
Prometheus server, Prometheus Operator, `ServiceMonitor`, or `PodMonitor`.

The Cloudflare API token requires:

- Account / Cloudflare Tunnel / Edit
- Account / Account Settings / Read
- Zone / DNS / Edit

The Cloudflare Operator is unofficial alpha software. Removing its
`ClusterTunnel` through Flux deletes the remote tunnel; removing its
`TunnelBinding` deletes the DNS record.

## References

- [Cloudflare Operator](https://github.com/adyanth/cloudflare-operator)
- [ESO Doppler provider](https://external-secrets.io/latest/provider/doppler/)
- [Flux post-build substitution](https://fluxcd.io/flux/components/kustomize/kustomizations/#post-build-variable-substitution)
- [CNPG Barman Cloud plugin](https://cloudnative-pg.io/plugin-barman-cloud/docs/usage/)
- [Tailscale Kubernetes Operator](https://tailscale.com/docs/kubernetes-operator/install-operator)
- [Grafana Alloy on Kubernetes](https://grafana.com/docs/alloy/latest/configure/kubernetes/)
