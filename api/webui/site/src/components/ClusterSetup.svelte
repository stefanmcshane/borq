<script lang="ts">
  import { requestButton } from "../lib/actions";
  import { truth, variant } from "../lib/format";
  import type { clusterSetupData } from "../lib/models";
  export let data: clusterSetupData;
</script>

<div class="cluster-setup-summary">
  {#if truth(data.Problem)}<p class="field-help" role="status">
      {data.Problem}
    </p>
    <button
      class="btn"
      data-variant="outline"
      data-size="sm"
      type="button"
      use:requestButton={{
        url: "/api/ui/cluster-setup",
        method: "GET",
        target: "closest .cluster-setup-summary",
      }}>Try again</button
    >
  {:else}
    <p class="field-help">
      Installed on the cluster you link, in the <strong class="mono"
        >{data.Setup.namespace}</strong
      > namespace.
    </p>
    <dl class="cluster-setup-list">
      {#each data.Setup.components as component}<div>
          <dt>
            <strong>{component.name}</strong><span
              class="badge mono"
              data-variant="outline">{component.version}</span
            >
          </dt>
          <dd>{component.description}</dd>
        </div>{/each}
    </dl>
    <p class="field-help">
      These controllers also install cluster-wide permissions and resource
      definitions. Existing managed installations are reused.
    </p>
  {/if}
</div>
