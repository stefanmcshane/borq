<script lang="ts">
  import { truth } from "../lib/format";
  import type { clusterCapacityData } from "../lib/models";
  export let data: clusterCapacityData;
</script>

<section class="card usage-card cluster-capacity-card">
  <header>
    <h3>{data.Title}</h3>
    {#if truth(data.Available)}<span class="field-help"
        >{Number(data.Percent).toFixed(1)}% of allocatable</span
      >{/if}
  </header>
  <div class="usage-reading">
    <strong
      >{#if truth(data.Available)}{data.Value}{:else}Unavailable{/if}</strong
    >
  </div>
  {#if truth(data.Available)}<meter
      class="cluster-capacity-meter"
      min="0"
      max="100"
      value={data.Percent}
      aria-label={data.Title + " as a percentage of allocatable capacity"}
      >{Number(data.Percent).toFixed(1)}%</meter
    >{:else}<p class="field-help cluster-capacity-missing">
      Recent usage is required from every node to show a cluster total.
    </p>{/if}
  <footer>
    <span>Allocatable capacity</span><strong>{data.Capacity}</strong>
  </footer>
</section>
