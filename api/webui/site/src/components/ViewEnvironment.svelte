<script lang="ts">
  import EnvironmentSpaceRequired from "./EnvironmentSpaceRequired.svelte";
  import UiIcon from "./UiIcon.svelte";
  import { truth, pageUrl, variant } from "../lib/format";
  import type { pageData } from "../lib/models";
  export let data: pageData;
</script>

{#if !truth(data.Space.cluster)}<EnvironmentSpaceRequired {data} />{:else}
  <section class="card management-panel">
    <header class="panel-heading">
      <div>
        <h2>Secrets in {data.Space.name}</h2>
        <p>
          Manage application secret groups and container registry credentials.
        </p>
      </div>
      <a
        class="btn"
        href={pageUrl("environment-group", data.Organization, data.Space.name)}
        >Create a secret</a
      >
    </header>
    <section>
      {#if truth(data.Groups)}<div class="table-scroll">
          <table class="table">
            <thead
              ><tr
                ><th>Secret</th><th>Type</th><th>Latest version</th><th>Keys</th
                ></tr
              ></thead
            ><tbody
              >{#each data.Groups as group}<tr class="resource-row"
                  ><td
                    ><a
                      class="text-link resource-row-link"
                      href={pageUrl(
                        "environment-group",
                        data.Organization,
                        data.Space.name,
                        "group",
                        group.name,
                      )}>{group.name}</a
                    >{#if truth(group.managedBy)}
                      <span
                        class="badge"
                        data-variant="secondary"
                        title={"Managed by " + group.managedBy}>Managed</span
                      >{/if}</td
                  ><td>{group.type}</td><td
                    ><span class="badge" data-variant="outline"
                      >v{group.version}</span
                    ></td
                  ><td>{group.variableCount}</td></tr
                >{/each}</tbody
            >
          </table>
        </div>
      {:else}<div class="resource-empty">
          <UiIcon data={"management"} />
          <h3>No secrets yet</h3>
          <p>
            Create a secret group for application values or a registry secret
            for private images.
          </p>
        </div>{/if}
    </section>
  </section>
{/if}
