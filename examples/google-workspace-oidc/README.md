# Bootstrap Borq with Google Workspace sign-in

This example installs Borq on an existing Kubernetes cluster and configures Google
Workspace OIDC. Borq remains accessible with its administrator token throughout
setup. Arrange HTTPS access at `https://borq.example.com` before testing sign-in;
this minimal document does not configure DNS, networking, or certificates. For
Cloudflare and Tailscale networking, see the [complete setup example](../backup-with-cloudflare-tailscale/README.md).

## Set credentials and the first administrator

Edit [management.yaml](management.yaml):

- Set `borqPublicUrl` to your Borq HTTPS origin.
- Put your Google web application's client ID in `organizations[0].oidc.clientId`
  and its client secret in `clientSecret`.
- Set `adminEmail` to the Workspace account that will first administer this Borq
  organization. Use the account's actual primary email address.
- Keep `issuerUrl: https://accounts.google.com`. Google Workspace uses Google's
  shared OIDC issuer, rather than a URL based on your Workspace domain.

Use an OAuth application configured for your Workspace organization if you want
Google to restrict who can authenticate. Borq grants access to the selected
administrator and members assigned roles; an email domain alone grants no
Borq access. Google's [OIDC documentation](https://developers.google.com/identity/openid-connect/openid-connect)
covers obtaining the web client credentials.

## Bootstrap and save recovery keys

Run:

```sh
chmod 600 management.yaml
borq management bootstrap --kubeconfig /path/to/kubeconfig -f management.yaml --timeout 30m
borq management keys --kubeconfig /path/to/kubeconfig --output-file ./borq-management-recovery.json
```

Bootstrap installs Borq, waits for its supporting services, and applies external
OIDC settings through the private Kubernetes connection. Once the server accepts
the configuration, the CLI prints the OIDC callback and logout URLs before waiting
for the operation. Register the callback even if a later apply step fails.
If configuration fails,
Borq remains installed. Follow the keys command's private-access and login
instructions, correct the file, then run `borq management apply -f management.yaml`.

Keep the recovery file securely outside the cluster. It contains `borq.adminToken`
and OpenBao recovery material. Keep the administrator token for ongoing setup and
recovery, including after OIDC sign-in works. Keep the original configuration
private because it contains the Google client secret.

## Register the callback and sign in

Copy the `OIDC example callback` URL printed during bootstrap into your Google
web client's **Authorized redirect URIs**. Read saved settings again with
`borq org oidc get example` after following the keys command's login instructions.
The callback path includes an immutable organization identifier and the issuer,
so use the returned URL rather than constructing it yourself.

1. Follow the keys command's port-forward instructions. For a remote cluster,
   forward the local port over SSH to your browser machine as needed.
2. Open `/login/admin` on that private Borq address and sign in using
   `borq.adminToken` from the recovery file.
3. Select `example` and open **Admin → Authentication** to review settings. This
   page also previews the callback when you enter the issuer and Borq address,
   before saving credentials.
4. Open `https://borq.example.com/login`, choose `example`, and sign in with the
   `adminEmail` account. The first successful sign-in binds that verified account
   as this organization's initial administrator.

The initial administrator has full access within `example`. It does not become an
installation administrator. Additional accounts can sign in through the provider
and appear under **Admin → Members** without organization access. Create roles
and assign them to grant access. Google Workspace membership alone grants no
Borq permissions. The admin token continues to grant installation-wide access.

If Google needs the redirect URI before you can create client credentials,
bootstrap without `-f management.yaml`, retrieve the recovery keys, and log in
with the administrator token first. Create the organization and preview its URLs:

```sh
borq org create example
borq org oidc callback example --issuer-url https://accounts.google.com --borq-public-url https://borq.example.com
```

This command needs no OIDC credentials or provider connection. Register the
callback, add the client ID and secret to `management.yaml`, then run
`borq management apply -f management.yaml`.
Use the same callback command if an accepted setup is still queued and the
authentication form is locked.

Pocket ID is optional. Deploy it later as a managed application on an application
cluster, then connect it through OIDC using its issuer and client credentials.
