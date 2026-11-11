# Bootstrap Borq with Microsoft Entra ID

Edit [management.yaml](management.yaml) with your Borq HTTPS origin, Microsoft
tenant ID, application client ID, client secret **value**, and initial administrator
email. Use the tenant-specific issuer. Arrange HTTPS access separately; this
minimal example does not configure networking or certificates. For Cloudflare
and Tailscale, see the [complete setup example](../backup-with-cloudflare-tailscale/README.md).

Bootstrap and retain installation recovery access:

```sh
chmod 600 management.yaml
borq management bootstrap --kubeconfig /path/to/kubeconfig -f management.yaml --timeout 30m
borq management keys --kubeconfig /path/to/kubeconfig --output-file ./borq-management-recovery.json
```

Register each printed public and private callback as a **Web** redirect URI in your
Microsoft application. Follow the keys command's private access and CLI login
instructions using `borq.adminToken`. Read the callback again with:

```sh
borq org oidc get example
```

If you need the redirect URI before saving credentials, bootstrap without `-f`,
retrieve the keys, log in with the administrator token, then run:

```sh
borq org create example
borq org oidc callback example --issuer-url https://login.microsoftonline.com/YOUR_TENANT_ID/v2.0 --borq-public-url https://borq.example.com
```

Register that callback, fill in the credentials, and run
`borq management apply -f management.yaml`.

## Sign in as the first administrator

In your Microsoft application's ID-token optional claims, include `email` and
`xms_edov`, preserving any existing claims:

```json
"optionalClaims": {
  "idToken": [
    { "name": "email", "essential": false },
    { "name": "xms_edov", "essential": false }
  ]
}
```

Microsoft's [optional claims instructions](https://learn.microsoft.com/en-us/entra/identity-platform/optional-claims)
show where to save these settings. Borq handles Microsoft's verification claim
internally; you do not need to configure an account ID for normal setup.

After applying the configuration and registering the callback, open
`https://borq.example.com/login`, select `example`, and sign in as `adminEmail`.
Borq accepts Microsoft's signed domain-verification claim only from your exact
configured tenant, with a matching tenant ID. The first accepted sign-in pins the
issuer and account ID internally; later email changes do not change membership.
A matching email string, UPN, or tenant alone never grants access.

Microsoft documents the [email domain verification claim](https://learn.microsoft.com/en-us/entra/identity-platform/optional-claims-reference).
WorkOS's [identity-linking guidance](https://workos.com/docs/authkit/identity-linking)
explains why matching unverified email addresses can permit account takeover.
Keep the installation admin token for recovery after OIDC works.

## Recovery when the provider cannot verify email

If Microsoft omits `xms_edov` or returns false, Borq cannot automatically establish
ownership from the email. The installation administrator can explicitly approve
the first account:

1. Sign into `/login/admin` through the private Borq address with the admin token.
2. Select `example`, open **Admin → Authentication**, and use **Test sign-in**.
3. Authenticate as the intended administrator. Copy **Account ID** from Borq's
   successful test result.
4. Add that value as `organizations[0].oidc.adminSubject` alongside your existing
   issuer, client and email fields in `management.yaml`, then run:

   ```sh
   borq management apply -f management.yaml --timeout 30m
   ```

Use Borq's Account ID, not an email, UPN, or Microsoft directory object ID.
Management apply binds this account atomically with the configured provider.
A failed provider update leaves it unenrolled; correct the failure and reapply.
Export retains the account ID. Reapplying preserves existing grants and disabled
state, and cannot replace an enrolled administrator.

Additional provider accounts can sign in and appear under **Admin → Members**
without organization access. Create roles and assign them to grant access. An
email or tenant match does not assign roles. `adminSubject` selects only the
initial administrator.

For recovery outside management YAML, the installation administrator can also run
`borq org oidc enroll-admin example --subject 'ACCOUNT_ID_FROM_TEST_SIGN_IN'`.

Sign-in through the Tailscale management address uses its own callback URL.
Register that private callback alongside the public callback. The final
`borq management apply` summary and `borq org oidc get example` list both addresses.
