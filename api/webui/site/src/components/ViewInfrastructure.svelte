<script lang="ts">
  import type { pageData } from "../lib/models";
  import { loadPanel } from "../lib/actions";
  import { pageUrl } from "../lib/format";
  import { infrastructureTab, infrastructureUrl } from "../lib/infrastructure";
  import UiIcon from "./UiIcon.svelte";
  import InfrastructureCatalog from "./InfrastructureCatalog.svelte";
  import ViewSettings from "./ViewSettings.svelte";
  import ViewClusters from "./ViewClusters.svelte";
  import ComponentHost from "./ComponentHost.svelte";
  export let data: pageData;
  $: selection = data.Infrastructure;
  $: management = selection.Node === "management";
  $: tabs = management
    ? ["components", "clusters", "settings"]
    : ["components", "metrics", "spaces", "applications", "settings"];
</script>

<div class="application-identity infrastructure-identity">
  <div class="app-avatar"><UiIcon data="clusters" /></div>
  <div>
    <p class="app-image">
      {management ? "Management" : "Application cluster · " + data.Organization}
    </p>
    <h2>{management ? "Management cluster" : data.Cluster}</h2>
    <p class="field-help">
      {management
        ? `Manages ${data.ClusterSummaries.length} application clusters and shared services.`
        : `Managed by Management cluster · ${data.SelectedCluster.Spaces.length} spaces · ${data.SelectedCluster.ApplicationCount} applications`}
    </p>
  </div>
</div>
<nav class="application-tabs" aria-label="Cluster sections">
  {#each tabs as tab}<a
      href={infrastructureTab(selection, tab)}
      aria-current={selection.Tab === tab ? "page" : undefined}
      >{tab === "clusters"
        ? "Application clusters"
        : tab[0].toUpperCase() +
          tab.slice(
            1,
          )}{#if tab === "clusters" || tab === "spaces" || tab === "applications"}<span
          class="badge"
          data-variant="outline"
          >{tab === "clusters"
            ? data.ClusterSummaries.length
            : tab === "spaces"
              ? data.SelectedCluster.Spaces.length
              : data.SelectedCluster.ApplicationCount}</span
        >{/if}</a
    >{/each}
</nav>
{#if data.Settings.Operation.id}<div class="inline-operation">
    <ComponentHost
      refreshWorkspaceOnComplete={management && selection.Tab === "settings"}
      initial={{
        component: "management-operation",
        data: data.Settings.Operation,
      }}
    />
  </div>{/if}
{#if selection.Tab === "clusters"}<ViewClusters {data} />
{:else if selection.Tab === "components"}
  <InfrastructureCatalog {data} />
{:else if selection.Tab === "settings"}
  {#if management}<ViewSettings {data} />{:else}<section
      class="card management-panel"
    >
      <header>
        <h2>Cluster connection</h2>
        <p>This saved registration connects Borq to the application cluster.</p>
      </header>
      <section class="cluster-connection-content">
        <dl class="component-metadata cluster-connection-metadata">
          <div>
            <dt>Organization</dt>
            <dd>{data.Organization}</dd>
          </div>
          <div>
            <dt>Cluster</dt>
            <dd>{data.Cluster}</dd>
          </div>
          {#if data.SelectedCluster.Context}<div>
              <dt>Context</dt>
              <dd>{data.SelectedCluster.Context}</dd>
            </div>{/if}{#if data.SelectedCluster.ClientAPIServer}<div>
              <dt>Client API server</dt>
              <dd>{data.SelectedCluster.ClientAPIServer}</dd>
            </div>{/if}
          <div>
            <dt>Connection</dt>
            <dd>
              {data.SelectedCluster.InCluster
                ? "Server service account"
                : "Saved kubeconfig"}
            </dd>
          </div>
        </dl>
        <div class="form-actions">
          <a
            class="btn"
            data-variant="outline"
            href={"/api/ui/kubeconfig?" +
              new URLSearchParams({
                organization: data.Organization,
                cluster: data.Cluster,
              })}>Download kubeconfig</a
          >
        </div>
        <p class="field-help">
          Backup archive storage is shared. <a
            class="text-link"
            href={infrastructureUrl({
              Tab: "settings",
              ComponentScope: "postgres",
            })}>Manage shared settings</a
          >
        </p>
      </section>
    </section>{/if}
{:else if selection.Tab === "metrics"}<div class="section-heading">
    <div>
      <h2>Cluster health and capacity</h2>
      <p>
        Live node and workload observations across all namespaces · refreshes
        every 30 seconds
      </p>
    </div>
  </div>
  <div
    id="cluster-metrics"
    use:loadPanel={{ url: data.ClusterPath, lazy: false, interval: 0 }}
  >
    <p class="panel-loading" role="status">
      Reading cluster health and resource usage…
    </p>
  </div>
{:else if selection.Tab === "spaces"}<section class="card management-panel">
    <header><h2>Spaces on {data.Cluster}</h2></header>
    <section>
      {#if data.SelectedCluster.Spaces.length}<div class="table-scroll">
          <table class="table">
            <thead
              ><tr><th>Space</th><th>Namespace</th><th>Applications</th></tr
              ></thead
            ><tbody
              >{#each data.SelectedCluster.Spaces as space}<tr
                  class="resource-row"
                  ><td
                    ><a
                      class="text-link resource-row-link"
                      href={pageUrl("overview", data.Organization, space.name)}
                      >{space.name}</a
                    ></td
                  ><td>{space.namespace}</td><td
                    >{space.configuration.apps.length}</td
                  ></tr
                >{/each}</tbody
            >
          </table>
        </div>{:else}<p class="empty-copy">
          No spaces attached through this organization yet.
        </p>{/if}
    </section>
  </section>
{:else}<section class="card management-panel">
    <header><h2>Applications on {data.Cluster}</h2></header>
    <section>
      {#if data.SelectedCluster.ApplicationCount}<div class="table-scroll">
          <table class="table">
            <thead
              ><tr
                ><th>Application</th><th>Space</th><th>Image</th><th
                  >Replicas</th
                ></tr
              ></thead
            ><tbody
              >{#each data.SelectedCluster.Spaces as space}{#each space.configuration.apps as app}<tr
                    class="resource-row"
                    ><td
                      ><a
                        class="text-link resource-row-link"
                        href={pageUrl(
                          "application",
                          data.Organization,
                          space.name,
                          "app",
                          app.name,
                        )}>{app.name}</a
                      ></td
                    ><td>{space.name}</td><td>{app.image}</td><td
                      >{app.replicas}</td
                    ></tr
                  >{/each}{/each}</tbody
            >
          </table>
        </div>{:else}<p class="empty-copy">
          No applications in this cluster link’s spaces yet.
        </p>{/if}
    </section>
  </section>{/if}

<style>
  .cluster-connection-content {
    display: grid;
    gap: 16px;
  }

  .cluster-connection-metadata {
    grid-template-columns: repeat(2, minmax(0, 1fr));
    margin: 0;
  }

  .cluster-connection-metadata > div:first-child,
  .cluster-connection-metadata > div:nth-child(2) {
    grid-column: auto;
  }

  @media (max-width: 640px) {
    .cluster-connection-metadata {
      grid-template-columns: minmax(0, 1fr);
    }
  }
</style>
