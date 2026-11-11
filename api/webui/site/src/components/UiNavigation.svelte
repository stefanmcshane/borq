<script lang="ts">
  import { infrastructureUrl } from "../lib/infrastructure";
  import UiIcon from "./UiIcon.svelte";
  import UiSpaceNavigation from "./UiSpaceNavigation.svelte";
  import { pageUrl } from "../lib/format";
  import type { pageData } from "../lib/models";
  export let data: pageData;
  $: organization = data.Organization || data.Organizations[0]?.name || "";
  $: spaces = data.Spaces.length
    ? data.Spaces
    : data.ClusterSummaries.filter(
        (cluster) => cluster.Organization === organization,
      ).flatMap((cluster) => cluster.Spaces);
</script>

<a
  class="btn nav-item"
  data-variant="ghost"
  href={pageUrl("overview", organization, "")}
  aria-current={data.View === "overview" && !data.Space.id ? "page" : undefined}
  ><UiIcon data="overview" /><span>Overview</span></a
>
<p class="nav-group-label">Workspace</p>
<a
  class="btn nav-item"
  data-variant="ghost"
  href={pageUrl("spaces", organization, "")}
  aria-current={data.View === "spaces" ? "page" : undefined}
  ><UiIcon data="spaces" /><span>All spaces</span></a
>
<div class="spaces-tree" aria-label="Spaces">
  {#each spaces as space (space.id)}
    <UiSpaceNavigation {data} {organization} {space} />
  {/each}
</div>
{#if data.CanManageUsers || data.CanConfigureOrganization || data.ManagementAdmin}<p class="nav-group-label">Admin</p>{/if}
{#if organization && data.CanManageUsers}
  <a
    class="btn nav-item"
    data-variant="ghost"
    href={pageUrl("organization-members", organization, "")}
    aria-current={data.View === "organization-members" ? "page" : undefined}
  >
    <UiIcon data="management" /><span>Members</span>
  </a>
{/if}
{#if organization && data.CanConfigureOrganization}
  <a
    class="btn nav-item"
    data-variant="ghost"
    href={pageUrl("organization-authentication", organization, "")}
    aria-current={data.View === "organization-authentication"
      ? "page"
      : undefined}
  >
    <UiIcon data="management" /><span>Authentication</span>
  </a>
{/if}
{#if data.ManagementAdmin}<a
    class="btn nav-item"
    data-variant="ghost"
    href={infrastructureUrl()}
    aria-current={data.View === "infrastructure" &&
    data.Infrastructure.Node === "management"
      ? "page"
      : undefined}><UiIcon data="clusters" /><span>Infrastructure</span></a
  >{/if}
{#if data.View === "infrastructure"}<div
    class="infrastructure-tree"
    aria-label="Infrastructure tree"
  >
    <section aria-labelledby="application-clusters-label">
      <h2 id="application-clusters-label" class="infrastructure-group-label">
        Application clusters
      </h2>
      <div class="infrastructure-children">
        {#each data.ClusterSummaries as cluster}<a
            href={infrastructureUrl({
              Node: "application",
              Organization: cluster.Organization,
              Cluster: cluster.Name,
              ComponentScope: "target",
            })}
            aria-current={data.Infrastructure.Node === "application" &&
            data.Organization === cluster.Organization &&
            data.Cluster === cluster.Name
              ? "page"
              : undefined}
            ><strong>{cluster.Name}</strong><span>{cluster.Organization}</span
            ></a
          >{/each}
      </div>
    </section>
  </div>{/if}
