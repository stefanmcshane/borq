<script lang="ts">
  import UiScope from "./UiScope.svelte";
  import { loadPanel, submitForm } from "../lib/actions";
  import { truth, pageUrl, variant } from "../lib/format";
  import type { pageData } from "../lib/models";
  export let data: pageData;
</script>

<form
  class="form-grid"
  action="/api/ui/actions"
  method="post"
  autocomplete="off"
  use:submitForm={{ target: "#workspace" }}
>
  {#if data.View === "infrastructure"}
    <input type="hidden" name="view" value="infrastructure" /><input
      type="hidden"
      name="node"
      value="management"
    /><input type="hidden" name="tab" value="clusters" /><input
      type="hidden"
      name="componentScope"
      value="management"
    />
    <label
      >Organization<select class="select" name="organization" required
        ><option value="" disabled selected={!truth(data.Organization)}
          >Select an organization</option
        >{#each data.Organizations as organization}<option
            value={organization.name}
            selected={truth(data.Organization === organization.name)}
            >{organization.name}</option
          >{/each}</select
      ></label
    >
  {:else}<UiScope {data} />{/if}
  <input
    type="hidden"
    name="action"
    value="register-cluster"
  />{#if truth(data.PromptCluster)}<input
      type="hidden"
      name="setup"
      value="cluster"
    />{/if}<label
    >Cluster name<input
      class="input"
      name="name"
      required
      maxlength="63"
      pattern="[a-z0-9]([a-z0-9-]*[a-z0-9])?"
      placeholder="production"
    /></label
  ><label
    >Kubeconfig<textarea
      class="textarea code-editor"
      name="kubeconfig"
      rows="8"
      required
      placeholder="Paste a kubeconfig with embedded credentials"
      spellcheck="false"></textarea></label
  >
  <p class="field-help">
    The kubeconfig supplies the cluster connection. The server must be able to
    reach it. Credentials are saved privately.
  </p>
  <div class="cluster-setup">
    <p class="field-help">Linking includes required target component setup.</p>
    <details>
      <summary class="btn disclosure-button" data-variant="outline"
        >What gets installed and where</summary
      >
      <div
        class="cluster-setup-summary"
        use:loadPanel={{
          url: "/api/ui/cluster-setup",
          lazy: true,
          interval: 0,
        }}
      >
        <p class="field-help" role="status">Loading required components…</p>
      </div>
    </details>
  </div>
  <div class="form-actions">
    <button class="btn" type="submit">Link cluster</button
    >{#if truth(data.PromptCluster)}<a
        class="btn"
        data-variant="ghost"
        href={pageUrl("overview", data.Organization, "")}
        >I’ll link a cluster later</a
      >{/if}
  </div>
</form>
