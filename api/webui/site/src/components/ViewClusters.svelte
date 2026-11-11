<script lang="ts">
  import UiClusterForm from "./UiClusterForm.svelte";
  import UiClustersTable from "./UiClustersTable.svelte";
  import UiIcon from "./UiIcon.svelte";
  import { truth, variant } from "../lib/format";
  import type { pageData } from "../lib/models";
  export let data: pageData;
</script>

<div class="section-heading">
  <div>
    <h2>Application clusters</h2>
    <p>
      Select a cluster to inspect its infrastructure, spaces, and applications.
    </p>
  </div>
  {#if truth(data.Organizations)}<button
      class="btn"
      data-size="sm"
      type="button"
      data-dialog-open="cluster-registration-dialog"
      ><UiIcon data={"plus"} />Link a cluster</button
    >{/if}
</div>
{#if truth(data.ClusterSummaries)}
  <section class="card management-panel">
    <section><UiClustersTable {data} /></section>
  </section>
{:else}<section class="empty workspace-empty">
    <header>
      <div class="app-avatar"><UiIcon data={"clusters"} /></div>
      <h2>No clusters linked yet</h2>
      <p>
        Link your Kubernetes infrastructure to an organization to start
        deploying.
      </p>
      {#if !truth(data.Organizations)}<button
          class="btn"
          type="button"
          data-dialog-open="organization-dialog">Create organization</button
        >{/if}
    </header>
  </section>{/if}
{#if truth(data.Organizations)}<dialog
    id="cluster-registration-dialog"
    class="workspace-dialog"
    aria-labelledby="cluster-registration-title"
  >
    <header class="dialog-heading">
      <div>
        <h2 id="cluster-registration-title">Link a cluster</h2>
        <p>
          Choose its organization. Borq adds five characters to the cluster name
          and creates its initial space.
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
  </dialog>{/if}
