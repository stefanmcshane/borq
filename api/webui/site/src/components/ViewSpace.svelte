<script lang="ts">
  import UiIcon from "./UiIcon.svelte";
  import WorkloadMetrics from "./WorkloadMetrics.svelte";
  import WorkloadLogs from "./WorkloadLogs.svelte";
  import { infrastructureUrl } from "../lib/infrastructure";
  import { pageUrl } from "../lib/format";
  import type { pageData } from "../lib/models";
  export let data: pageData;
</script>

<div class="application-identity infrastructure-identity">
  <div class="app-avatar"><UiIcon data="spaces" /></div>
  <div>
    <p class="app-image">Space · {data.Organization}</p>
    <h2>{data.Space.name}</h2>
    <p class="field-help">{data.Space.cluster} · {data.Space.namespace}</p>
  </div>
</div>
<nav class="application-tabs" aria-label="Space sections">
  {#each ["overview", "metrics", "logs"] as tab}
    <a
      href={pageUrl("overview", data.Organization, data.Space.name, "tab", tab)}
      aria-current={data.SpaceTab === tab ? "page" : undefined}
      >{tab[0].toUpperCase() + tab.slice(1)}</a
    >
  {/each}
</nav>
{#if data.SpaceTab === "metrics"}
  <WorkloadMetrics data={data.Monitoring} />
{:else if data.SpaceTab === "logs"}
  <WorkloadLogs data={data.Monitoring} />
{:else}
  <div class="metrics-grid overview-metrics">
    <a
      class="card metric"
      href={pageUrl("applications", data.Organization, data.Space.name)}
      ><span>Applications</span><strong
        >{data.Space.configuration.apps.length}</strong
      ><small>View applications →</small></a
    >
    <a
      class="card metric"
      href={infrastructureUrl({
        Node: "application",
        Organization: data.Organization,
        Cluster: data.Space.cluster,
        Tab: "metrics",
        ComponentScope: "target",
      })}
      ><span>Cluster</span><strong class="metric-name"
        >{data.Space.cluster}</strong
      ><small>View cluster health →</small></a
    >
  </div>
  <section class="card management-panel">
    <header class="panel-heading">
      <div>
        <h2>Applications in {data.Space.name}</h2>
        <p>Accepted configuration for this space.</p>
      </div>
      <a
        class="btn"
        data-variant="outline"
        href={pageUrl("applications", data.Organization, data.Space.name)}
        >Manage applications</a
      >
    </header>
    <section>
      {#if data.Space.configuration.apps.length}
        <div class="table-scroll">
          <table class="table">
            <thead
              ><tr><th>Application</th><th>Image</th><th>Replicas</th></tr
              ></thead
            >
            <tbody
              >{#each data.Space.configuration.apps as app}<tr
                  class="resource-row"
                >
                  <td
                    ><a
                      class="text-link resource-row-link"
                      href={pageUrl(
                        "application",
                        data.Organization,
                        data.Space.name,
                        "app",
                        app.name,
                      )}>{app.name}</a
                    ></td
                  >
                  <td>{app.image}</td><td>{app.replicas}</td>
                </tr>{/each}</tbody
            >
          </table>
        </div>
      {:else}<p class="empty-copy">No applications in this space yet.</p>{/if}
    </section>
  </section>
{/if}
