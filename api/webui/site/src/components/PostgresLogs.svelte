<script lang="ts">
  import { truth, timeLabel } from "../lib/format";
  import type { postgresPanelData } from "../lib/models";
  export let data: postgresPanelData;
</script>

<div id="postgres-logs" class="logs-panel">
  {#if truth(data.Problem)}<p class="field-help" role="status">
      {data.Problem}
    </p>{:else if truth(data.Logs.entries)}<div
      class="log-output"
      tabindex="0"
      role="textbox"
      aria-readonly="true"
      aria-multiline="true"
      aria-label="PostgreSQL logs"
    >
      {#each data.Logs.entries as entrie}<div class="log-line">
          <time>{timeLabel(entrie.timestamp)}</time><span class="log-source"
            >{entrie.pod} / {entrie.container}</span
          ><span class="log-message">{entrie.message}</span>
        </div>{/each}
    </div>
    {#if truth(data.Logs.limitReached)}<p class="field-help">
        Showing the latest 200 matching lines. Narrow the history or filter text
        for earlier output.
      </p>{/if}{:else}<p class="empty-copy">
      No matching output in this history window.
    </p>{/if}
</div>
