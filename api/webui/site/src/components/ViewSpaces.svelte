<script lang="ts">
  import UiIcon from "./UiIcon.svelte";
  import UiScope from "./UiScope.svelte";
  import UiSpacesList from "./UiSpacesList.svelte";
  import { submitForm } from "../lib/actions";
  import { truth, pageUrl, variant } from "../lib/format";
  import type { pageData } from "../lib/models";
  export let data: pageData;
</script>

<section class="card management-panel">
  <header class="panel-heading">
    <div>
      <h2>Spaces in {data.Organization}</h2>
      <p>Select a space to manage its applications and configuration.</p>
    </div>
    {#if truth(data.Clusters)}<button
        class="btn"
        type="button"
        data-dialog-open="space-creation-dialog">Create a space</button
      >{/if}
  </header>
  <section><UiSpacesList {data} /></section>
</section>
{#if truth(data.Clusters)}
  <dialog
    id="space-creation-dialog"
    class="workspace-dialog"
    aria-labelledby="space-creation-title"
  >
    <header class="dialog-heading">
      <div>
        <h2 id="space-creation-title">Create a space</h2>
        <p>Choose the cluster where this space will run.</p>
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
      use:submitForm={{}}
    >
      <UiScope {data} /><input
        type="hidden"
        name="action"
        value="create-space"
      /><label
        >Space name<input
          class="input"
          name="name"
          required
          maxlength="63"
          pattern="[a-z0-9]([a-z0-9-]*[a-z0-9])?"
          placeholder="staging"
          data-focus
        /></label
      ><label
        >Cluster<select class="select" name="cluster" required
          ><option value="" disabled selected>Select a cluster</option
          >{#each data.Clusters as cluster}<option value={cluster.name}
              >{cluster.name}</option
            >{/each}</select
        ></label
      >
      <div class="form-actions">
        <button class="btn" type="submit">Create space</button><button
          class="btn"
          data-variant="ghost"
          type="button"
          data-dialog-close>Cancel</button
        >
      </div>
    </form>
  </dialog>
{:else}
  <section class="alert">
    <h2>Link a cluster before creating a space</h2>
    <p>Every space needs a cluster to run its applications.</p>
    <a
      class="btn"
      data-variant="outline"
      href={pageUrl("clusters", data.Organization, "")}>Link a cluster</a
    >
  </section>
{/if}
