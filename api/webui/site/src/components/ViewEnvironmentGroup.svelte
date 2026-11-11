<script lang="ts">
  import SecretDelete from "./SecretDelete.svelte";
  import EnvironmentGroupApplications from "./EnvironmentGroupApplications.svelte";
  import EnvironmentGroupSettings from "./EnvironmentGroupSettings.svelte";
  import EnvironmentGroupVariables from "./EnvironmentGroupVariables.svelte";
  import EnvironmentGroupVersions from "./EnvironmentGroupVersions.svelte";
  import EnvironmentSpaceRequired from "./EnvironmentSpaceRequired.svelte";
  import UiIcon from "./UiIcon.svelte";
  import { truth, pageUrl, terminal, variant } from "../lib/format";
  import type { pageData } from "../lib/models";
  export let data: pageData;
</script>

{#if !truth(data.Space.cluster)}<EnvironmentSpaceRequired {data} />{:else}
  {#if truth(data.GroupLatestVersion)}
    <section
      class="application-identity environment-group-identity"
      aria-label="Secret details"
    >
      <div class="app-avatar"><UiIcon data={"terminal"} /></div>
      <div>
        <p class="app-image">{data.Group.name}</p>
        <p class="field-help">
          {data.Secret.type === "registry"
            ? "Container registry"
            : "Secret group"} · Latest v{data.GroupLatestVersion} · Used by {data
            .GroupApplications.length}
          {#if data.GroupApplications.length === 1}application{:else}applications{/if}
        </p>
        {#if truth(data.Group.managedBy)}<p class="field-help">
            <span class="badge" data-variant="secondary">Managed</span> Managed
            by {data.Group.managedBy}. Values refresh automatically.
          </p>{/if}
      </div>
    </section>
    <nav class="application-tabs" aria-label="Secret sections">
      <a
        href={pageUrl(
          "environment-group",
          data.Organization,
          data.Space.name,
          "group",
          data.Group.name,
        )}
        aria-current={truth(data.GroupTab === "variables") ? "page" : undefined}
        >Variables</a
      >
      {#if data.Secret.type !== "registry"}<a
          href={pageUrl(
            "environment-group",
            data.Organization,
            data.Space.name,
            "group",
            data.Group.name,
            "tab",
            "versions",
          )}
          aria-current={truth(data.GroupTab === "versions")
            ? "page"
            : undefined}>Versions</a
        >
        <a
          href={pageUrl(
            "environment-group",
            data.Organization,
            data.Space.name,
            "group",
            data.Group.name,
            "tab",
            "applications",
          )}
          aria-current={truth(data.GroupTab === "applications")
            ? "page"
            : undefined}
          >Applications <span class="badge" data-variant="secondary"
            >{data.GroupApplications.length}</span
          ></a
        >
      {/if}
      <a
        href={pageUrl(
          "environment-group",
          data.Organization,
          data.Space.name,
          "group",
          data.Group.name,
          "tab",
          "settings",
        )}
        aria-current={truth(data.GroupTab === "settings") ? "page" : undefined}
        >Settings</a
      >
    </nav>
  {/if}
  {#if data.GroupTab === "versions"}<EnvironmentGroupVersions {data} />
  {:else if data.GroupTab === "applications"}<EnvironmentGroupApplications
      {data}
    />
  {:else if data.GroupTab === "settings"}{#if data.Secret.type !== "registry"}<EnvironmentGroupSettings
        {data}
      />{/if}<SecretDelete {data} />
  {:else}{#key [data.Organization, data.Space.id, data.Group.name, data.GroupLatestVersion].join("/")}<EnvironmentGroupVariables
        {data}
      />{/key}{/if}
{/if}
