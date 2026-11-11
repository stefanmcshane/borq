<script lang="ts">
  import ViewSpace from "./ViewSpace.svelte";
  import UiSpacesList from "./UiSpacesList.svelte";
  import { truth, pageUrl } from "../lib/format";
  import type { pageData } from "../lib/models";
  export let data: pageData;
</script>

{#if truth(data.Space.id)}
  <ViewSpace {data} />
{:else if truth(data.Spaces)}
  <div class="metrics-grid overview-metrics">
    <a class="card metric" href={pageUrl("spaces", data.Organization, "")}
      ><span>Spaces</span><strong>{data.Spaces.length}</strong><small
        >Manage spaces →</small
      ></a
    ><a class="card metric" href={pageUrl("clusters", data.Organization, "")}
      ><span>Clusters</span><strong>{data.Clusters.length}</strong><small
        >Manage clusters →</small
      ></a
    >
    <div class="card metric">
      <span>Applications across all spaces</span><strong
        >{data.ApplicationCount}</strong
      >
    </div>
  </div>
  <section class="card management-panel">
    <header>
      <h2>Spaces in {data.Organization}</h2>
      <p>Select a space to open its overview.</p>
    </header>
    <section><UiSpacesList {data} /></section>
  </section>
{:else}
  <section class="card management-panel">
    <header>
      <h2>Ready for your first application</h2>
      <p>
        {#if truth(data.Clusters)}Create a space to choose where your
          applications run.{:else}Link a cluster to start deploying
          applications.{/if}
      </p>
    </header>
    <section>
      {#if truth(data.Clusters)}<a
          class="btn"
          href={pageUrl("spaces", data.Organization, "")}>Create a space</a
        >{:else}<a class="btn" href={pageUrl("clusters", data.Organization, "")}
          >Link a cluster</a
        >{/if}
    </section>
  </section>
{/if}
