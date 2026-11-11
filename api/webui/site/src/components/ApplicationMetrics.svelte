<script lang="ts">
  import UsageChart from "./UsageChart.svelte";
  import { requestButton } from "../lib/actions";
  import { truth, variant } from "../lib/format";
  import type { applicationPanelData } from "../lib/models";
  export let data: applicationPanelData;
</script>

<div
  id="application-metrics"
  class="usage-panel"
  data-selected-range={data.Range}
>
  {#if truth(data.Problem)}<div class="monitoring-empty">
      <strong>Metrics unavailable</strong>
      <p>{data.Problem}</p>
      <button
        class="btn"
        data-variant="outline"
        type="button"
        use:requestButton={{
          url: data.Location,
          method: "GET",
          target: "#application-metrics",
        }}>Try again</button
      >
    </div>{:else}
    <div class="usage-charts">
      <UsageChart data={data.Cpu} /><UsageChart data={data.Memory} />
    </div>
    {#if data.Endpoint !== "/api/ui/space"}<p class="field-help chart-legend">
        <span class="event-key" data-kind="deployment"></span> Deployment
        <span class="event-key" data-kind="restart"></span>
        Restart / warning <span class="event-key" data-kind="lifecycle"></span> Pod
        lifecycle · Hover or focus a graph to inspect values and events.
      </p>
    {/if}
    {#if truth(data.EventsNotice)}<p class="field-help" role="status">
        {data.EventsNotice}
      </p>{/if}
    <p class="field-help chart-footnote">
      {#if data.Range === "30m"}Last 30 minutes · 30-second intervals · CPU
        averaged over 2 minutes{:else if data.Range === "1h"}Last hour ·
        30-second intervals · CPU averaged over 2 minutes{:else if data.Range === "12h"}Last
        12 hours · 5-minute averages{:else}Last 3 days · 30-minute averages{/if}.
      1,000 mCPU = 1 CPU core. Gaps mean no samples were collected.{#if data.Endpoint !== "/api/ui/space"}
        Pod events begin when lifecycle collection is enabled.{/if}
    </p>
  {/if}
</div>
