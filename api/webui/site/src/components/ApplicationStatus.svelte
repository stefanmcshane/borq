<script lang="ts">
  import { truth } from "../lib/format";
  import type { applicationPanelData } from "../lib/models";
  export let data: applicationPanelData;
</script>

<span
  class="app-status"
  {...!truth(data.Problem) ? { "data-state": data.Details.state } : {}}
  ><span class="status-dot" data-state={data.Details.state}
  ></span>{#if truth(data.Problem)}Health unavailable{:else}{data.Details
      .state}{/if}</span
>
