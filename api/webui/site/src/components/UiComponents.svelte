<script lang="ts">
  import { truth, componentLabel, variant } from "../lib/format";
  import type { componentData } from "../lib/models";
  export let data: componentData;
</script>

<section class="card management-panel components-panel">
  <header class="panel-heading">
    <div>
      <h2>{data.Title}</h2>
      <p>{componentLabel(data.Installation.message)}</p>
    </div>
    <span class="badge" data-variant={variant(data.Installation.status)}
      >{componentLabel(data.Installation.status)}</span
    >
  </header>
  <section>
    {#if truth(data.Problem)}<p class="empty-copy">
        {data.Problem}
      </p>{/if}{#if truth(data.Installation.result.components)}<div
        class="table-scroll"
      >
        <table class="table">
          <thead
            ><tr
              ><th>Component</th><th>Version</th><th>Helm revision</th><th
                >Status</th
              ></tr
            ></thead
          ><tbody
            >{#each data.Installation.result.components as component}<tr
                ><td>{component.name}</td><td class="mono"
                  >{component.version}</td
                ><td class="mono"
                  >{#if component.revision > 0}{component.revision}{:else}—{/if}</td
                ><td
                  ><span class="badge" data-variant={variant(component.status)}
                    >{componentLabel(component.status)}</span
                  ></td
                ></tr
              >{/each}</tbody
          >
        </table>
      </div>{/if}
  </section>
</section>
