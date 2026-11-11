<script lang="ts">
  import UiIcon from "./UiIcon.svelte";
  import UiPlan from "./UiPlan.svelte";
  import { loadPanel } from "../lib/actions";
  import ComponentHost from "./ComponentHost.svelte";
  import { truth, pageUrl, appPanelUrl, variant } from "../lib/format";
  import type { pageData } from "../lib/models";
  export let data: pageData;
</script>

{#if truth(data.Operation.id)}<ComponentHost
    initial={{ component: "ui-operation", data: data.Operation }}
    refreshWorkspaceOnComplete={data.Operation.phase === "deleting"}
    recoveryActions={[
      {
        label: "View space logs",
        href: pageUrl(
          "overview",
          data.Organization,
          data.Space.name,
          "tab",
          "logs",
        ),
      },
      {
        label: "Review applications",
        href: pageUrl("applications", data.Organization, data.Space.name),
      },
    ]}
  />{/if}
{#if truth(data.Plan.id)}<UiPlan {data} />{/if}
{#if truth(data.Space.id)}
  <section class="card management-panel">
    <header class="panel-heading">
      <div>
        <h2>Your applications</h2>
        <p>Select an application to manage its settings and deployment.</p>
      </div>
      {#if truth(data.Space.cluster)}<a
          class="btn"
          href={pageUrl("application", data.Organization, data.Space.name)}
          >Add an application</a
        >{/if}
    </header>
    <section>
      {#if truth(data.Applications.configuration.apps)}
        <div class="application-list">
          {#each data.Applications.configuration.apps as app}<article
              class="application-list-item resource-row"
            >
              <a
                class="app-avatar"
                href={pageUrl(
                  "application",
                  data.Organization,
                  data.Space.name,
                  "app",
                  app.name,
                )}
                aria-label={"Manage " + app.name}
                ><UiIcon data={"applications"} /></a
              >
              <div class="application-list-main">
                <a
                  class="application-name resource-row-link"
                  href={pageUrl(
                    "application",
                    data.Organization,
                    data.Space.name,
                    "app",
                    app.name,
                  )}>{app.name}</a
                >
                <p class="app-image mono">{app.image}</p>
                <p class="field-help">
                  {#if truth(app.port)}Web · port {app.port}{:else}Worker{/if} · {app.replicas}
                  replicas{#if truth(app.env.name)}
                    · {app.env.name} v{app.env.version}{/if}
                </p>
              </div>
              <div
                class="application-list-health"
                use:loadPanel={{
                  url: appPanelUrl(
                    data.Organization,
                    data.Space.name,
                    app.name,
                    "status",
                  ),
                  lazy: false,
                  interval: 30000,
                }}
              >
                <span class="field-help">Checking health…</span>
              </div>
              <a
                class="btn"
                data-variant="outline"
                href={pageUrl(
                  "application",
                  data.Organization,
                  data.Space.name,
                  "app",
                  app.name,
                )}>Manage →</a
              >
            </article>{/each}
        </div>
      {:else}
        <div class="resource-empty">
          <div class="app-avatar"><UiIcon data={"applications"} /></div>
          <h3>Your next application starts here</h3>
          <p>
            Deploy a container image, connect a domain, and watch it run. Logs,
            resource metrics, and deployment history come with every app.
          </p>
          <a
            class="btn"
            href={pageUrl("application", data.Organization, data.Space.name)}
            >Add an application</a
          >
        </div>
      {/if}
    </section>
  </section>
{/if}
{#if !truth(data.Space.cluster)}<section class="alert">
    <h2>Create a space before deploying</h2>
    <p>Choose a registered cluster when creating a space on the Spaces page.</p>
    <a
      class="btn"
      data-variant="outline"
      href={pageUrl("spaces", data.Organization, data.Space.name)}
      >Create space</a
    >
  </section>{/if}
