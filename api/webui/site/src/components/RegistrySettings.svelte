<script lang="ts">
  import InfrastructureSelection from "./InfrastructureSelection.svelte";
  import UiScope from "./UiScope.svelte";
  import { submitForm } from "../lib/actions";
  import { terminal } from "../lib/format";
  import type { pageData } from "../lib/models";

  export let data: pageData;
  export let action: "put-space-registry" | "put-management-registry";
  $: management = action === "put-management-registry";
  $: pending =
    Boolean(data.Settings.Operation.id) &&
    !terminal(data.Settings.Operation.status);
</script>

<section class="card management-panel" aria-label="Container registry">
  <header class="panel-heading">
    <div>
      <h2>Container registry</h2>
      <p>
        {management
          ? "Pull private management server images from GitHub Container Registry."
          : `Pull private container images for applications in ${data.Space.name}.`}
      </p>
    </div>
    <span class="badge" data-variant="outline">
      {data.Registry.Configured ? "Configured" : "Not configured"}
    </span>
  </header>
  <p class="field-help">
    {management
      ? "Application spaces use their own registry credentials."
      : "These credentials apply only to this space, including its deployment tasks."}
  </p>
  {#key [action, data.Organization, data.Space.id, data.Settings.Operation.id].join("/")}
    <form
      class="form-grid"
      action="/api/ui/actions"
      method="post"
      autocomplete="off"
      use:submitForm={{}}
    >
      {#if management}
        <InfrastructureSelection data={data.Infrastructure} />
      {:else}
        <UiScope {data} />
      {/if}
      <input type="hidden" name="action" value={action} />
      <fieldset class="registry-fields form-grid" disabled={pending}>
        <label
          >GitHub username
          <input
            class="input"
            name="githubUsername"
            required
            spellcheck="false"
            autocomplete="off"
          />
        </label>
        <label>
          <span id="registry-token-label">GitHub personal access token</span>
          <input
            class="input"
            type="password"
            name="githubToken"
            required
            autocomplete="new-password"
            spellcheck="false"
            aria-labelledby="registry-token-label"
            aria-describedby="registry-token-help"
          />
          <small id="registry-token-help"
            >Use a personal access token (classic) with read:packages access to
            your private images.</small
          >
        </label>
        <p class="field-help">
          Your token is stored securely and is never shown again.
          {#if data.Registry.Configured}Enter both fields to replace the saved
            credentials.{/if}
        </p>
        <div class="form-actions">
          <button class="btn" type="submit">
            {data.Registry.Configured
              ? "Replace GHCR credentials"
              : "Save GHCR credentials"}
          </button>
        </div>
      </fieldset>
    </form>
  {/key}
</section>

<style>
  .registry-fields {
    border: 0;
    margin: 0;
    min-width: 0;
    padding: 0;
  }
</style>
