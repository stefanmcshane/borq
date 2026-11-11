<script lang="ts">
  import { onMount } from "svelte";
  import { submitForm } from "../lib/actions";
  import type { pageData } from "../lib/models";

  export let data: pageData;
  let origin = "";
  let callbackPreview = "";
  let previewRequest = 0;
  onMount(() => {
    origin = window.location.origin;
  });
  $: issuerUrl = data.Oidc.issuerUrl;
  $: borqPublicUrl = data.Oidc.borqPublicUrl || origin;
  $: logoutCallbackPreview = borqPublicUrl.trim()
    ? `${borqPublicUrl.trim().replace(/\/+$/, "")}/login`
    : data.Oidc.logoutCallbackUrl;
  $: additionalCallbacks = data.Oidc.callbackUrls.filter((url) => url !== data.Oidc.callbackUrl);
  $: additionalLogoutCallbacks = data.Oidc.logoutCallbackUrls.filter((url) => url !== data.Oidc.logoutCallbackUrl);
  $: onboardingPending =
    data.Organizations.find((org) => org.name === data.Organization)
      ?.authenticationState === "queued";
  $: pocketIdPending =
    onboardingPending ||
    ["queued", "provisioning"].includes(data.PocketId.state);
  $: organizationId =
    data.Organizations.find((org) => org.name === data.Organization)?.id || "";
  $: void previewCallback(issuerUrl, borqPublicUrl, organizationId);

  async function previewCallback(
    issuer: string,
    address: string,
    organization: string,
  ) {
    const request = ++previewRequest;
    callbackPreview = "";
    if (
      !issuer.trim() ||
      !address.trim() ||
      !organization ||
      typeof window === "undefined"
    )
      return;
    try {
      const digest = await window.crypto.subtle.digest(
        "SHA-256",
        new TextEncoder().encode(`${organization}/${issuer.trim()}`),
      );
      const id = Array.from(new Uint8Array(digest).slice(0, 16), (byte) =>
        byte.toString(16).padStart(2, "0"),
      ).join("");
      if (request === previewRequest)
        callbackPreview = `${address.trim().replace(/\/+$/, "")}/api/auth/oidc/callback/${id}`;
    } catch {
      // The saved callback remains available on browsers without Web Crypto.
      if (
        request === previewRequest &&
        issuer === data.Oidc.issuerUrl &&
        address === data.Oidc.borqPublicUrl
      )
        callbackPreview = data.Oidc.callbackUrl;
    }
  }
</script>

<div class="section-heading">
  <div>
    <h2>Organization authentication</h2>
    <p>Connect {data.Organization} to an OpenID Connect provider.</p>
  </div>
</div>

{#if onboardingPending}
  <p class="field-help" role="status">
    Borq is configuring this organization's sign-in provider in the background.
    Refresh this page to check progress. Administrator recovery access remains
    available.
  </p>
{/if}

{#if data.PocketId.state === "ready"}
  <p class="field-help">
    Pocket ID's My Apps page launches applications. To edit Borq's client
    settings, open Administration → OIDC Clients in Pocket ID.
  </p>
{/if}

<section class="card management-panel">
  <header>
    <h2>OIDC provider</h2>
    <p>
      Use Pocket ID, Google, Microsoft Entra ID, or another OIDC provider. Each
      organization has its own settings.
    </p>
    <p class="field-help">
      Normal sign-in uses this provider. Members appear after their first login;
      an organization administrator assigns their roles. A settings test checks
      the callback without signing you in.
    </p>
  </header>
  {#if data.ManagementAdmin}
    {#key `${data.Organization}:${data.Oidc.revision}`}
      <form
        aria-label="OIDC provider settings"
        class="form-grid"
        action="/api/ui/actions"
        method="post"
        autocomplete="off"
        use:submitForm={{}}
      >
        <input type="hidden" name="action" value="put-organization-oidc" />
        <input type="hidden" name="view" value="organization-authentication" />
        <input type="hidden" name="organization" value={data.Organization} />
        <input type="hidden" name="revision" value={data.Oidc.revision} />
        <label>
          Administrator email
          <input
            class="input"
            type="email"
            name="adminEmail"
            maxlength="254"
            required
            value={data.Oidc.adminEmail}
            disabled={pocketIdPending}
            placeholder="owner@example.com"
          />
        </label>
        <p class="field-help">
          Select the verified provider account that will administer this organization.
          Other provider-approved accounts start without access until an
          administrator assigns a role.
        </p>
        <label
          >Issuer URL
          <input
            class="input"
            type="url"
            name="issuerUrl"
            required
            maxlength="2048"
            bind:value={issuerUrl}
            disabled={pocketIdPending}
            placeholder="https://id.example.com"
          />
        </label>
        <p class="field-help">
          Use the provider's issuer URL, not its discovery-document or sign-in
          URL. For Entra ID, use a tenant-specific issuer.
        </p>
        <label
          >Client ID
          <input
            class="input"
            name="clientId"
            required
            maxlength="1024"
            value={data.Oidc.clientId}
            disabled={pocketIdPending}
            autocomplete="off"
          />
        </label>
        <label
          >Client secret
          <input
            class="input"
            type="password"
            name="clientSecret"
            maxlength="8192"
            required={!data.Oidc.clientSecretConfigured}
            disabled={pocketIdPending}
            autocomplete="new-password"
          />
        </label>
        <p class="field-help">
          {data.Oidc.clientSecretConfigured
            ? "A secret is saved. Leave this blank to keep it. Enter a new secret if you change the issuer or client ID."
            : "Create a confidential web application at your provider, then enter its client secret."}
          Borq stores the secret in management OpenBao and never displays it.
        </p>
        <label
          >Borq public URL
          <input
            class="input"
            type="url"
            name="borqPublicUrl"
            required
            maxlength="2048"
            bind:value={borqPublicUrl}
            disabled={pocketIdPending}
            placeholder="https://borq.example.com"
          />
        </label>
        <p class="field-help">
          Enter the address you use to open Borq, without a path. HTTPS is
          required, except for local development on localhost or a loopback
          address.
        </p>
        {#if callbackPreview}
          <label
            >Callback URL to register at your provider
            <input
              class="input"
              readonly
              value={callbackPreview}
              aria-describedby="oidc-callback-help"
            />
          </label>
          <p class="field-help" id="oidc-callback-help">
            Register this exact redirect URI when creating the provider's web
            application. It changes if you change the issuer or Borq public URL.
          </p>
        {/if}
        {#if logoutCallbackPreview}
          <label
            >Logout callback URL to register at your provider
            <input
              class="input"
              readonly
              value={logoutCallbackPreview}
              aria-describedby="oidc-logout-callback-help"
            />
          </label>
          <p class="field-help" id="oidc-logout-callback-help">
            Register this post-logout redirect URI to return to Borq's sign-in
            page. It changes when you change the Borq public URL.
          </p>
        {/if}
        <div class="form-actions">
          <button class="btn" type="submit" disabled={pocketIdPending}
            >Save and check discovery</button
          >
        </div>
      </form>
    {/key}
  {:else}
    <p class="field-help">
      Provider configuration requires management administrator access.
    </p>
    <div class="form-grid">
      <label
        >Issuer URL<input
          class="input"
          readonly
          value={data.Oidc.issuerUrl}
        /></label
      >
      <label
        >Client ID<input
          class="input"
          readonly
          value={data.Oidc.clientId}
        /></label
      >
      <label
        >Borq public URL<input
          class="input"
          readonly
          value={data.Oidc.borqPublicUrl}
        /></label
      >
      <label
        >Callback URL to register at your provider<input
          class="input"
          readonly
          value={data.Oidc.callbackUrl}
        /></label
      >
      <label
        >Logout callback URL to register at your provider<input
          class="input"
          readonly
          value={data.Oidc.logoutCallbackUrl}
        /></label
      >
    </div>
  {/if}
</section>

{#if additionalCallbacks.length}
  <section class="card management-panel">
    <header><h2>Private sign-in callbacks</h2><p>Register these addresses at your identity provider as well to sign in through the private management address.</p></header>
    <div class="form-grid">
      {#each additionalCallbacks as callback}
        <label>Private callback URL<input class="input" readonly value={callback} /></label>
      {/each}
      {#each additionalLogoutCallbacks as callback}
        <label>Private logout callback URL<input class="input" readonly value={callback} /></label>
      {/each}
    </div>
  </section>
{/if}

{#if data.ManagementAdmin}
  <section class="card management-panel">
    <header>
      <h2>Test sign-in</h2>
      <p>
        After saving, sign in at your provider to verify the client credentials
        and callback. This does not create a Borq session or grant access.
      </p>
    </header>
    {#if data.Oidc.callbackUrl && !callbackPreview}
      <label
        >Callback URL to register at your provider
        <input class="input" readonly value={data.Oidc.callbackUrl} />
      </label>
      <p class="field-help">Register this exact redirect URI before testing.</p>
    {/if}
    {#if data.Oidc.lastTestedAt}
      <p class="field-help" role="status">
        Last successful test: {new Date(
          data.Oidc.lastTestedAt,
        ).toLocaleString()}.
      </p>
    {:else}
      <p class="field-help">
        These settings have not completed a sign-in test.
      </p>
    {/if}
    <form
      action={`/api/auth/oidc/test/${encodeURIComponent(data.Organization)}`}
      method="post"
    >
      <button
        class="btn"
        data-variant="outline"
        type="submit"
        disabled={!data.Oidc.clientSecretConfigured}
        >Test saved sign-in settings</button
      >
    </form>
  </section>
{/if}
