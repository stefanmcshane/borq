# PostgreSQL backups with Cloudflare and Tailscale

Use an existing Borq management installation and a logged-in CLI. Follow the
[main setup instructions](../../README.md#development-and-deployment-testing)
first if needed. This example runs management, the frontend, the backend and
PostgreSQL on the same cluster.

Copy these files to a private working directory before adding credentials.

## Configure the example

- In `management.yaml`, replace the domain, administrator email, Cloudflare zone
  and API token, Tailscale OAuth credentials, and GitHub username and token.
  Set the Google client ID, client secret and initial administrator in the
  organization's `oidc` block. Copy the callback URL printed during apply into
  the Google web client's authorized redirect URIs. It is printed before waiting
  for apply to finish. To retrieve it independently, use `borq org oidc callback
  example --issuer-url https://accounts.google.com --borq-public-url https://borq.example.com`
  after the organization exists and you have logged in with the administrator token.
  Sign in with the configured initial administrator account.
  The GitHub token needs `read:packages` access to both private images.
- Merge `tailscale-policy.hujson` into your tailnet policy. Enable MagicDNS and
  HTTPS certificates, and authorize the OAuth client for
  `tag:borq-management-operator`. Follow the
  [operator credential requirements](https://tailscale.com/docs/kubernetes-operator/install-operator).
- Give the Cloudflare token Zone Read and DNS Edit for your zone, plus
  Cloudflare One Connector: cloudflared Write for its account. See
  [networking configuration](../../docs/networking.md).
- In `offsite-recovery.json`, set the existing external S3 bucket, region,
  credentials and independently retained Restic password.
- In `apps.yaml`, replace the GHCR images, hostnames, ports and health paths.
  Configure the frontend to use the backend URL and the backend to allow the
  frontend origin. Adjust PostgreSQL capacity in `database.yaml` as needed.

## Apply the configuration

```sh
chmod 600 management.yaml offsite-recovery.json
borq management apply -f management.yaml --recovery-config offsite-recovery.json --timeout 30m
borq cluster reconcile management --organization example --timeout 15m
borq am postgres apply -f database.yaml --organization example --space production --timeout 30m
borq am postgres get appdb --organization example --space production
```

Copy the database's `environmentGroup` value into `apps.yaml`, replacing
`postgres-appdb-replace-with-database-id`. Keep `version: latest` so the backend
follows managed password rotation. The group supplies `DATABASE_URL` and the
standard `PGHOST`, `PGPORT`, `PGDATABASE`, `PGUSER` and `PGPASSWORD` variables.

```sh
borq apply -f apps.yaml --organization example --space production --wait
borq management backups status
borq management export > management-safe.yaml
```

After the first successful offsite export, copy `offsite.status.source` from the
safe export into `offsite-recovery.json`. Retain the source identity for each
space and Pocket ID bucket too. They have separate snapshot histories.

## Backup coverage and recovery

Management SQLite and OpenBao snapshots run hourly, retaining 24 artifacts per
component in local RustFS. PostgreSQL uses daily base backups, continuous WAL
archiving, a seven-day recovery window and scheduled restore checks. Offsite
exports copy supported archives, including configured Pocket ID SQLite backups,
to external S3 every six hours, retaining 28 successful snapshots per source.
Check each database's coverage rather than relying on offsite status alone.

Keep the original management configuration, offsite recovery configuration and
exported `borq-management-recovery.json` in secure storage outside the cluster.
Arbitrary uploaded files, image registries and Kubernetes resources require
their own recovery arrangements. See the [S3 disaster recovery example](../restore-from-s3/README.md).

Only one installation may write to a live database, backup prefix or Restic
source history at a time. Fence the original installation before starting a
replacement. Two installations writing to the same locations can overwrite
data or prune each other's backups.
