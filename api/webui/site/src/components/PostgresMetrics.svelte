<script lang="ts">
  import UsageChart from "./UsageChart.svelte";
  import { truth, bytesLabel } from "../lib/format";
  import type { postgresPanelData } from "../lib/models";
  export let data: postgresPanelData;
</script>

<section id="postgres-metrics" class="management-panel">
  {#if truth(data.Problem)}<p class="field-help" role="status">
      {data.Problem}
    </p>{:else}<div class="usage-charts">
      <UsageChart data={data.Cpu} /><UsageChart data={data.Memory} />
    </div>
    <section class="card management-panel">
      <header><h2>Disk and query activity</h2></header>
      <section>
        <dl class="release-metadata">
          <div>
            <dt>Database size</dt>
            <dd>
              {#if truth(data.Metrics.activityAvailable)}{bytesLabel(
                  data.Metrics.diskBytes,
                )}{:else}Unavailable{/if}
            </dd>
          </div>
          <div>
            <dt>Filesystem usage</dt>
            <dd>
              {#if truth(data.Metrics.diskAvailable)}{bytesLabel(
                  data.Metrics.diskUsedBytes,
                )} / {bytesLabel(
                  data.Metrics.diskCapacityBytes,
                )}{:else}Unavailable{/if}
            </dd>
          </div>
          <div>
            <dt>Free filesystem space</dt>
            <dd>
              {#if truth(data.Metrics.diskAvailable)}{bytesLabel(
                  data.Metrics.diskAvailableBytes,
                )}{:else}Unavailable{/if}
            </dd>
          </div>
          <div>
            <dt>Connections</dt>
            <dd>
              {#if truth(data.Metrics.activityAvailable)}{data.Metrics
                  .connections}{:else}Unavailable{/if}
            </dd>
          </div>
          <div>
            <dt>Active queries</dt>
            <dd>
              {#if truth(data.Metrics.activityAvailable)}{data.Metrics
                  .activeQueries}{:else}Unavailable{/if}
            </dd>
          </div>
          <div>
            <dt>Transactions</dt>
            <dd>
              {#if truth(data.Metrics.activityAvailable)}{data.Metrics
                  .transactions}{:else}Unavailable{/if}
            </dd>
          </div>
        </dl>
        <p class="field-help">
          Filesystem capacity may be shared with other volumes when using local
          storage.
        </p>
        {#if truth(data.Metrics.notice)}{@const value1 = data.Metrics.notice}
          <p class="field-help" role="status">{value1}</p>{/if}
      </section>
    </section>{/if}
</section>
