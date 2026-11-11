<script lang="ts">
  import UiIcon from "./UiIcon.svelte";
  import { truth, pageUrl, variant } from "../lib/format";
  import type { pageData } from "../lib/models";
  export let data: pageData;
</script>

{#if truth(data.Space.id)}
  <section class="card management-panel">
    <header class="panel-heading">
      <div>
        <h2>Custom addons</h2>
        <p>
          Install a Helm chart using its repository, name, version, and values.
        </p>
      </div>
      <a
        class="btn"
        href={pageUrl("custom-chart", data.Organization, data.Space.name)}
        >Install a custom addon</a
      >
    </header>
    <section>
      {#if truth(data.CustomCharts)}<div class="table-scroll">
          <table class="table">
            <thead
              ><tr
                ><th>Addon</th><th>Chart</th><th>Version</th><th>Status</th></tr
              ></thead
            ><tbody
              >{#each data.CustomCharts as addon}<tr class="resource-row"
                  ><td
                    ><a
                      class="text-link resource-row-link"
                      href={pageUrl(
                        "custom-chart",
                        data.Organization,
                        data.Space.name,
                        "customChart",
                        addon.name,
                      )}>{addon.name}</a
                    ></td
                  ><td>{addon.specification.chart}</td><td class="mono"
                    >{addon.specification.version}</td
                  ><td
                    ><span class="badge" data-variant={variant(addon.state)}
                      >{addon.state}</span
                    ></td
                  ></tr
                >{/each}</tbody
            >
          </table>
        </div>{:else}<div class="resource-empty">
          <UiIcon data="applications" />
          <h3>No custom addons yet</h3>
          <p>Install a Helm chart to add a service to this space.</p>
        </div>{/if}
    </section>
  </section>
{:else}<p class="empty-copy">Create a space to install custom addons.</p>{/if}
