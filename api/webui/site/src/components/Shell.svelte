<script lang="ts">
  import UiSidebar from "./UiSidebar.svelte";
  import UiTopbar from "./UiTopbar.svelte";
  import Workspace from "./Workspace.svelte";

  import type { pageData } from "../lib/models";
  export let data: pageData;
  export let renderGeneration: number;
</script>

<title>Borq · {data.Title}</title>
<div class="shell">
  <UiSidebar {data} />
  <div class="content">
    {#key renderGeneration}
      <UiTopbar {data} />
      <Workspace {data} />
    {/key}
    <footer class="footer">
      <span>Borq</span>
      <nav aria-label="Developer resources">
        <a href="/api/docs/">API reference</a>
        <a href="/api/mcp">MCP</a>
      </nav>
    </footer>
  </div>
</div>
