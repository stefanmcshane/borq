<script lang="ts">
  import UiClusterForm from "./UiClusterForm.svelte";
  import UiIcon from "./UiIcon.svelte";
  import { submitForm } from "../lib/actions";
  import { truth, variant } from "../lib/format";
  import type { pageData } from "../lib/models";
  export let data: pageData;
  let provider = "none";
</script>

<dialog
  id="organization-dialog"
  class="workspace-dialog"
  aria-labelledby="organization-dialog-title"
>
  <header class="dialog-heading">
    <div>
      <p class="dialog-step">1 · Create an organization</p>
      <h2 id="organization-dialog-title">New organization</h2>
      <p>
        Organize your spaces and applications, and attach the clusters they use.
      </p>
    </div>
    <button
      class="btn"
      data-variant="ghost"
      data-size="icon-sm"
      type="button"
      data-dialog-close
      aria-label="Close dialog"><UiIcon data={"close"} /></button
    >
  </header>
  <form
    class="form-grid"
    action="/api/ui/actions"
    method="post"
    use:submitForm={{ target: "#workspace" }}
  >
    <input type="hidden" name="action" value="create-organization" /><label
      >Organization name<input
        class="input"
        name="name"
        required
        maxlength="63"
        pattern="[a-z0-9]([a-z0-9-]*[a-z0-9])?"
        placeholder="my-team"
        autocomplete="off"
        data-focus
      /></label
    >
    <p class="field-help">Use lowercase letters, numbers, and hyphens.</p>
    <label for="organization-sign-in-provider">Sign-in provider</label>
    <select
      id="organization-sign-in-provider"
      class="select"
      name="provider"
      bind:value={provider}
    >
      <option value="none">Use admin token for now</option>
      <option value="reuse">Reuse an existing Pocket ID</option>
      <option value="external">Use external OIDC</option>
      <option value="new-pocketid">Customize a new Pocket ID</option>
    </select>
    {#if provider !== "none"}
      <label>
        {provider === "reuse"
          ? "Existing organization administrator email"
          : "Organization administrator email"}
        <input
          class="input"
          name="adminEmail"
          type="email"
          required={provider === "external" || provider === "reuse"}
          autocomplete="email"
          placeholder="owner@example.com"
        />
      </label>
      <p class="field-help">
        Select the verified account allowed to administer this Borq
        organization. This grants no Pocket ID administration and creates no
        provider account. A Pocket ID management administrator must create an
        ordinary account first if the selected account does not exist. Leave
        blank to use the designated Pocket ID management administrator.
      </p>
    {/if}
    {#if provider === "none"}
      <p class="field-help">
        Continue using your admin token. Configure OIDC under Admin →
        Authentication when you are ready. Pocket ID can be deployed separately
        as a managed application.
      </p>
    {:else if provider === "reuse"}
      <label>
        Pocket ID organization
        <input
          class="input"
          name="pocketIdOrganization"
          required
          placeholder="Organization owning the existing instance"
        />
      </label>
      <p class="field-help">
        Borq registers a separate application in the selected instance. Existing
        Pocket ID roles remain unchanged.
      </p>
    {:else}
      <label
        >Borq public URL<input
          class="input"
          name="borqPublicUrl"
          type="url"
          required
          placeholder="https://borq.example.com"
        /></label
      >
      <label
        >{provider === "external"
          ? "OIDC issuer address"
          : "Pocket ID address"}<input
          class="input"
          name="issuerUrl"
          type="url"
          required
          placeholder="https://id.example.com"
        /></label
      >
      {#if provider === "external"}
        <label
          >Client ID<input
            class="input"
            name="clientId"
            required
            autocomplete="off"
          /></label
        >
        <label
          >Client secret<input
            class="input"
            name="clientSecret"
            type="password"
            required
            autocomplete="new-password"
          /></label
        >
      {:else}
        <label>
          Pocket ID management administrator email
          <input
            class="input"
            name="pocketIdAdminEmail"
            type="email"
            required
            autocomplete="email"
          />
        </label>
        <label
          >Certificate issuer<input
            class="input"
            name="certificateIssuer"
            placeholder="letsencrypt-cloudflare"
          /></label
        >
        <p class="field-help">
          Pocket ID setup continues in the background. Keep the public Borq
          address for sign-in callbacks. Provider administration applies to the
          entire Pocket ID instance.
        </p>
      {/if}
    {/if}
    <div class="form-actions">
      <button class="btn" type="submit">Create organization</button><button
        class="btn"
        data-variant="ghost"
        type="button"
        data-dialog-close>Cancel</button
      >
    </div>
  </form>
</dialog>
{#if truth(data.PromptCluster)}
  <dialog
    id="cluster-link-dialog"
    class="workspace-dialog"
    aria-labelledby="cluster-link-dialog-title"
    data-auto-open
  >
    <header class="dialog-heading">
      <div>
        <p class="dialog-step">2 · Link your infrastructure</p>
        <h2 id="cluster-link-dialog-title">Link a cluster</h2>
        <p>
          {data.Organization} is ready. Link a cluster to create its first deployment
          space.
        </p>
      </div>
      <button
        class="btn"
        data-variant="ghost"
        data-size="icon-sm"
        type="button"
        data-dialog-close
        aria-label="Close dialog"><UiIcon data={"close"} /></button
      >
    </header>
    <UiClusterForm {data} />
  </dialog>
{/if}
