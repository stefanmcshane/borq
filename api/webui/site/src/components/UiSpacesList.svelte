<script lang="ts">
  import UiIcon from "./UiIcon.svelte";
  import { truth, pageUrl, variant } from "../lib/format";
  import type { pageData } from "../lib/models";
  export let data: pageData;
</script>

{#if truth(data.Spaces)}<div class="application-list">
    {#each data.Spaces as space}<article
        class="application-list-item resource-row"
      >
        <a
          class="app-avatar"
          href={pageUrl("overview", data.Organization, space.name)}
          aria-label={"Open " + space.name + " space"}
          ><UiIcon data={"spaces"} /></a
        >
        <div class="application-list-main">
          <a
            class="application-name resource-row-link"
            href={pageUrl("overview", data.Organization, space.name)}
            >{space.name}</a
          >
          <p class="app-image">
            {#if truth(space.cluster)}Cluster · {space.cluster}{:else}Unbound{/if}
          </p>
          <p class="field-help">
            {space.configuration.apps.length}
            {#if space.configuration.apps.length === 1}application{:else}applications{/if}
          </p>
        </div>
        <a
          class="btn"
          data-variant="outline"
          href={pageUrl("overview", data.Organization, space.name)}>Manage →</a
        >
      </article>{/each}
  </div>{:else}<div class="resource-empty">
    <div class="app-avatar"><UiIcon data={"spaces"} /></div>
    <h3>No spaces yet</h3>
    <p>Create a space to choose where your applications run.</p>
  </div>{/if}
