<script lang="ts">
  import { infrastructureUrl } from "../lib/infrastructure";
  import { componentLabel } from "../lib/format";
  import UiIcon from "./UiIcon.svelte";
  import { truth, pageUrl, variant } from "../lib/format";
  import type { pageData } from "../lib/models";
  export let data: pageData;
</script>

{#if data.View === "infrastructure"}<a href={infrastructureUrl()}
    >Infrastructure</a
  >{#if data.Infrastructure.Node === "application"}<span
      class="breadcrumb-divider">›</span
    ><span>{data.Organization} / {data.Cluster}</span>{/if}<span
    class="breadcrumb-divider">›</span
  ><span aria-current="page"
    >{data.Infrastructure.Tab === "clusters"
      ? "Application clusters"
      : componentLabel(data.Infrastructure.Tab)}</span
  >
{:else}
  <span class="breadcrumb-icon"><UiIcon data={"overview"} /></span>
  {#if data.SectionView === "clusters"}
    <span class="breadcrumb-name">Admin</span>
  {:else}
    <details class="breadcrumb-dropdown">
      <summary aria-label="Switch organization"
        >Org <UiIcon data={"chevron-down"} /></summary
      >
      <div class="breadcrumb-menu">
        <div class="breadcrumb-menu-heading">
          <span class="breadcrumb-menu-label">Organizations</span><button
            class="btn"
            data-variant="ghost"
            data-size="icon-sm"
            type="button"
            data-dialog-open="organization-dialog"
            aria-label="Add organization"
            title="Add organization"><UiIcon data={"plus"} /></button
          >
        </div>
        {#each data.Organizations as organization}<a
            href={pageUrl("overview", organization.name, "")}
            aria-current={truth(data.Organization === organization.name)
              ? "true"
              : undefined}
            >{organization.name}{#if data.Organization === organization.name}<UiIcon
                data={"check"}
              />{/if}</a
          >{/each}
        {#if truth(data.Organization)}<div
            class="breadcrumb-menu-separator"
          ></div>
          <a href={pageUrl("overview", data.Organization, "")}
            >Organization overview <UiIcon data={"overview"} /></a
          >{/if}
      </div>
    </details>
    {#if truth(data.Organization)}
      <span class="breadcrumb-divider" aria-hidden="true">›</span>
      <a
        class="breadcrumb-name"
        href={pageUrl("overview", data.Organization, "")}>{data.Organization}</a
      >
      {#if truth(data.Spaces) && data.View !== "spaces" && data.View !== "organization-authentication" && data.View !== "organization-members"}
        <span class="breadcrumb-divider" aria-hidden="true">›</span>
        <details class="breadcrumb-dropdown">
          <summary aria-label="Switch space"
            >Space <UiIcon data={"chevron-down"} /></summary
          >
          <div class="breadcrumb-menu">
            <span class="breadcrumb-menu-label">Spaces</span
            >{#each data.Spaces as space}<a
                href={pageUrl(data.SectionView, data.Organization, space.name)}
                aria-current={truth(data.Space.name === space.name)
                  ? "true"
                  : undefined}
                >{space.name}{#if data.Space.name === space.name}<UiIcon
                    data={"check"}
                  />{/if}</a
              >{/each}
            <div class="breadcrumb-menu-separator"></div>
            <a href={pageUrl("spaces", data.Organization, "")}
              >All spaces <UiIcon data={"spaces"} /></a
            >
          </div>
        </details>
        {#if truth(data.Space.id)}<span
            class="breadcrumb-divider"
            aria-hidden="true">›</span
          ><a
            class="breadcrumb-name"
            href={pageUrl("overview", data.Organization, data.Space.name)}
            >{data.Space.name}</a
          >{/if}
      {/if}
    {/if}
  {/if}
  <span class="breadcrumb-divider" aria-hidden="true">›</span>
  <div class="breadcrumb-section">
    {#if truth(data.SectionView === "applications") && truth(data.Space.id)}
      <details class="breadcrumb-dropdown">
        <summary
          aria-label="Switch application"
          aria-current={truth(data.View === data.SectionView)
            ? "page"
            : undefined}>Applications <UiIcon data={"chevron-down"} /></summary
        >
        <div class="breadcrumb-menu breadcrumb-application-menu">
          <span class="breadcrumb-menu-label">Applications</span
          >{#each data.Applications.configuration.apps as app}<a
              href={pageUrl(
                "application",
                data.Organization,
                data.Space.name,
                "app",
                app.name,
              )}
              aria-current={truth(data.Application.name === app.name)
                ? "true"
                : undefined}
              >{app.name}{#if data.Application.name === app.name}<UiIcon
                  data={"check"}
                />{/if}</a
            >{:else}<p class="breadcrumb-menu-empty">
              No applications in this space yet.
            </p>{/each}
          <div class="breadcrumb-menu-separator"></div>
          <a href={pageUrl("applications", data.Organization, data.Space.name)}
            >Manage applications <UiIcon data={"applications"} /></a
          >
        </div>
      </details>
    {:else if truth(data.SectionView === "custom-charts") && truth(data.Space.id)}
      <details class="breadcrumb-dropdown">
        <summary
          aria-label="Switch custom addon"
          aria-current={truth(data.View === data.SectionView)
            ? "page"
            : undefined}>Custom addons <UiIcon data={"chevron-down"} /></summary
        >
        <div class="breadcrumb-menu breadcrumb-addon-menu">
          <span class="breadcrumb-menu-label">Custom addons</span
          >{#each data.CustomCharts as addon}<a
              href={pageUrl(
                "custom-chart",
                data.Organization,
                data.Space.name,
                "customChart",
                addon.name,
              )}
              aria-current={truth(data.CustomChart.name === addon.name)
                ? "true"
                : undefined}
              >{addon.name}{#if data.CustomChart.name === addon.name}<UiIcon
                  data={"check"}
                />{/if}</a
            >{:else}<p class="breadcrumb-menu-empty">
              No custom addons in this space yet.
            </p>{/each}
          <div class="breadcrumb-menu-separator"></div>
          <a href={pageUrl("custom-charts", data.Organization, data.Space.name)}
            >Manage custom addons <UiIcon data={"applications"} /></a
          >
        </div>
      </details>
    {:else if data.View === "overview" && data.Space.id}
      <a
        href={pageUrl(
          "overview",
          data.Organization,
          data.Space.name,
          "tab",
          data.SpaceTab,
        )}
        aria-current="page"
        >{data.SpaceTab[0].toUpperCase() + data.SpaceTab.slice(1)}</a
      >
    {:else if data.SectionView === "clusters"}
      <a
        href={pageUrl("clusters", "", "")}
        aria-current={truth(data.View === "clusters") ? "page" : undefined}
        >Clusters</a
      >
    {:else}
      <a
        href={pageUrl(data.SectionView, data.Organization, data.Space.name)}
        aria-current={truth(data.View === data.SectionView)
          ? "page"
          : undefined}>{data.SectionTitle}</a
      >
    {/if}
  </div>
  {#if data.View !== data.SectionView}<span
      class="breadcrumb-divider"
      aria-hidden="true">›</span
    ><span class="breadcrumb-current" aria-current="page">{data.Title}</span
    >{/if}
{/if}
