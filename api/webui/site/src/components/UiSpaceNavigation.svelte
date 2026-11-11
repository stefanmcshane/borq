<script lang="ts">
  import UiIcon from "./UiIcon.svelte";
  import { pageUrl } from "../lib/format";
  import type { pageData, spaceSummary } from "../lib/models";
  export let data: pageData;
  export let organization: string;
  export let space: spaceSummary;
  $: selected = data.Space.id === space.id;
  let expanded = data.Space.id === space.id;
  $: if (selected) expanded = true;
  const sections = [
    { view: "applications", label: "Applications", icon: "applications" },
    { view: "postgres", label: "Managed addons", icon: "database" },
    { view: "custom-charts", label: "Custom addons", icon: "applications" },
    { view: "environment", label: "Secrets", icon: "spaces" },
  ];
</script>

<div class="space-branch" role="group" aria-label={space.name + " space"}>
  <div class="space-heading" data-selected={selected || undefined}>
    <button
      class="space-toggle"
      type="button"
      aria-label={(expanded ? "Collapse " : "Expand ") + space.name + " space"}
      aria-expanded={expanded}
      aria-controls={"space-navigation-" + space.id}
      onclick={() => (expanded = !expanded)}
      ><UiIcon data="chevron-down" /></button
    >
    <a
      class="space-name"
      href={pageUrl("overview", organization, space.name)}
      aria-current={selected && data.View === "overview" ? "page" : undefined}
      ><UiIcon data="spaces" /><span>{space.name}</span></a
    >
  </div>
  <div
    id={"space-navigation-" + space.id}
    class="space-sections"
    hidden={!expanded}
  >
    {#each sections as section}
      <a
        class="btn nav-item"
        data-variant="ghost"
        href={pageUrl(section.view, organization, space.name)}
        aria-current={selected && data.SectionView === section.view
          ? "page"
          : undefined}
        ><UiIcon data={section.icon} /><span>{section.label}</span></a
      >
    {/each}
  </div>
</div>

<style>
  .space-branch {
    min-width: 0;
  }
  .space-heading {
    display: flex;
    align-items: center;
    min-width: 0;
    border-radius: var(--radius);
    color: var(--muted-foreground);
  }
  .space-heading[data-selected] {
    color: var(--foreground);
  }
  .space-heading:has(a[aria-current="page"]),
  .space-heading:hover {
    background: var(--accent);
    color: var(--foreground);
  }
  .space-toggle {
    display: flex;
    align-items: center;
    justify-content: center;
    flex: 0 0 28px;
    width: 28px;
    height: 32px;
    padding: 0;
    border: 0;
    background: transparent;
    color: inherit;
    border-radius: var(--radius);
    cursor: pointer;
  }
  .space-toggle :global(.ui-icon) {
    width: 12px;
    height: 12px;
    transform: rotate(-90deg);
  }
  .space-toggle[aria-expanded="true"] :global(.ui-icon) {
    transform: rotate(0);
  }
  .space-name {
    display: flex;
    align-items: center;
    gap: 8px;
    flex: 1;
    min-width: 0;
    padding: 8px 8px 8px 0;
    font-size: 12px;
  }
  .space-name span {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .space-sections {
    display: grid;
    gap: 2px;
    margin: 3px 0 8px 19px;
    padding-left: 9px;
    border-left: 1px solid var(--border);
  }
  .space-sections[hidden] {
    display: none;
  }
  .space-sections .nav-item {
    min-width: 0;
    font-size: 11px;
    gap: 8px;
    padding: 0 7px;
  }
  @media (max-width: 540px) {
    .space-branch {
      flex: 1 1 170px;
    }
    .space-toggle {
      display: flex;
    }
    .space-sections .nav-item {
      width: 100%;
    }
  }
</style>
