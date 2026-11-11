<script lang="ts">
  import { loadPanel } from "../lib/actions";
  import { truth, variant, formatTimestamp, timestampKey } from "../lib/format";
  import type { applicationLogRow } from "../lib/models";
  export let data: applicationLogRow;
</script>

<div class="log-event" data-kind={data.Event!.kind}>
  <time
    datetime={formatTimestamp(
      data.Timestamp,
      "2006-01-02T15:04:05.999999999Z07:00",
    )}>{formatTimestamp(data.Timestamp, "Jan 02 15:04:05")}</time
  >
  <div class="log-event-message">
    <span class="log-event-copy"
      >{data.Event!.message}{#if truth(data.Event!.revision)}
        <span class="badge mono" data-variant="outline"
          >{data.Event!.revision}</span
        >{/if}{#if truth(data.Event!.pod)}
        · {data.Event!.pod}{/if}</span
    >{#if truth(data.Event!.deployment)}<details
        class="log-deployment-details"
        id={"log-deployment-" +
          data.Event!.deployment +
          "-" +
          timestampKey(data.Timestamp)}
        data-deployment-details
      >
        <summary>Deployment details</summary>
        <div
          class="inline-operation"
          use:loadPanel={{ url: data.OperationPath, lazy: true, interval: 0 }}
        >
          <p class="field-help" role="status">Loading deployment details…</p>
        </div>
      </details>{/if}
  </div>
</div>
