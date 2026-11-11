<script lang="ts">
  import UiBreadcrumb from "./UiBreadcrumb.svelte";
  import UiRefreshButton from "./UiRefreshButton.svelte";

  import type { pageData } from "../lib/models";
  export let data: pageData;
</script>

<header class="topbar">
  <nav id="workspace-breadcrumb" class="breadcrumb" aria-label="Breadcrumb">
    <UiBreadcrumb {data} />
  </nav>
  <div id="workspace-actions" class="topbar-actions">
    <UiRefreshButton {data} />
  </div>
</header>
