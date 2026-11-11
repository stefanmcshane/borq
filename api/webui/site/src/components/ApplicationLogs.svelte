<script lang="ts">
  import WorkloadEvent from "./WorkloadEvent.svelte";
  import { requestButton } from "../lib/actions";
  import { truth, variant, formatTimestamp } from "../lib/format";
  import type { applicationPanelData } from "../lib/models";
  export let data: applicationPanelData;
</script>

<div id="application-logs" class="logs-panel">
  <div class="logs-caption">
    <span
      >{#if truth(data.Follow)}Auto-refreshing every 5s{:else}Snapshot ·
        auto-refresh paused{/if}</span
    ><span
      >{#if truth(data.Logs.entries)}{data.Logs.entries.length} lines · newest last{:else}Recent
        output{/if}</span
    >
  </div>
  {#if truth(data.Problem)}<div class="monitoring-empty">
      <strong>Logs unavailable</strong>
      <p>{data.Problem}</p>
      <button
        class="btn"
        data-variant="outline"
        type="button"
        use:requestButton={{
          url: data.Location,
          method: "GET",
          target: "#application-logs",
        }}>Try again</button
      >
    </div>
  {:else if truth(data.Rows)}<div
      class="log-output"
      tabindex="0"
      role="textbox"
      aria-readonly="true"
      aria-multiline="true"
      aria-label="Pod logs and events"
      data-log-output
    >
      {#each data.Rows as row}{#if truth(row.Event)}<WorkloadEvent
            data={row}
          />{:else}<div class="log-line">
            <time
              datetime={formatTimestamp(
                row.Timestamp,
                "2006-01-02T15:04:05.999999999Z07:00",
              )}>{formatTimestamp(row.Timestamp, "Jan 02 15:04:05")}</time
            ><span
              class="log-source"
              title={"Replica " +
                row.Entry!.pod +
                " / " +
                row.Entry!.container +
                " \u00b7 image " +
                row.Entry!.image +
                " \u00b7 revision " +
                row.Entry!.revision +
                " \u00b7 rollout " +
                row.Entry!.replicaSet}
              ><span class="log-replica"
                >{row.Entry!.pod} / {row.Entry!.container}</span
              ><span class="log-version"
                >{#if truth(row.Entry!.revision)}rev {row.Entry!
                    .revision}{/if}{#if truth(row.Entry!.replicaSet)}
                  · rollout {row.Entry!
                    .replicaSet}{:else if !truth(row.Entry!.revision)}revision
                  unavailable{/if}{#if truth(row.Entry!.image)}
                  · {row.Entry!.image}{/if}</span
              ></span
            ><span class="log-message">{row.Entry!.message}</span>
          </div>{/if}{/each}
    </div>
    {#if truth(data.Search)}<p class="field-help">
        {#if data.Endpoint === "/api/ui/space"}Text filter applies to collected
          pod output in this space.{:else}Text filter applies to pod output.
          Deployment and pod events stay visible for context.{/if}
      </p>{/if}{#if truth(data.Logs.limitReached)}<p class="log-limit">
        Showing the latest 200 matching lines. Narrow the time range or filter
        by text to find earlier output.
      </p>{/if}
  {:else}<div class="monitoring-empty">
      <strong
        >{#if truth(data.Search)}No matching log lines{:else}No logs yet{/if}</strong
      >
      <p>
        {#if truth(data.Search)}Try a different filter or a longer time range.{:else}Pod
          output will appear here when collection is enabled and the workload
          writes to stdout or stderr.{/if}
      </p>
    </div>{/if}{#if truth(data.Logs.eventsNotice)}<p
      class="field-help"
      role="status"
    >
      {data.Logs.eventsNotice}
    </p>{/if}
</div>
