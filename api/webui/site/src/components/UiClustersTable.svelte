<script lang="ts">
  import { infrastructureUrl } from "../lib/infrastructure";
  import UiIcon from "./UiIcon.svelte";
  import { pageUrl } from "../lib/format";
  import type { pageData } from "../lib/models";
  export let data: pageData;
</script>

<div class="table-scroll">
  <table class="table cluster-table">
    <thead
      ><tr
        ><th>Cluster</th><th>Organization</th><th>Spaces</th><th
          >Applications</th
        ></tr
      ></thead
    ><tbody
      >{#each data.ClusterSummaries as cluster}<tr class="resource-row"
          ><td
            ><a
              class="text-link resource-row-link cluster-name"
              href={infrastructureUrl({
                Node: "application",
                Organization: cluster.Organization,
                Cluster: cluster.Name,
                ComponentScope: "target",
              })}
              ><span class="app-avatar"><UiIcon data={"clusters"} /></span
              ><strong>{cluster.Name}</strong></a
            ></td
          ><td
            ><a
              class="text-link"
              href={pageUrl("overview", cluster.Organization, "")}
              >{cluster.Organization}</a
            ></td
          ><td>{cluster.Spaces.length}</td><td>{cluster.ApplicationCount}</td
          ></tr
        >{/each}</tbody
    >
  </table>
</div>
