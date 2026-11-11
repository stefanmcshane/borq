<script lang="ts">
  import ClusterCapacity from "./ClusterCapacity.svelte";
  import { requestButton } from "../lib/actions";
  import { truth, timeLabel, variant } from "../lib/format";
  import type { clusterMetricsData } from "../lib/models";
  export let data: clusterMetricsData;
</script>

<div id="cluster-metrics" class="cluster-metrics">
  {#if truth(data.Problem)}<div class="monitoring-empty">
      <strong>Cluster metrics unavailable</strong>
      <p>{data.Problem}</p>
      <button
        class="btn"
        data-variant="outline"
        type="button"
        use:requestButton={{
          url: data.Location,
          method: "GET",
          target: "#cluster-metrics",
        }}>Try again</button
      >
    </div>
  {:else}
    <div class="metrics-grid cluster-health-grid">
      <div class="card metric">
        <span>Ready nodes</span><strong
          >{data.ReadyNodes}
          <small>/ {data.Snapshot.nodes.length}</small></strong
        ><small>Nodes reporting Ready</small>
      </div>
      <div class="card metric">
        <span>Active pods</span><strong>{data.ActivePods}</strong><small
          >of {data.PodCapacity} allocatable pod slots</small
        >
      </div>
      <div class="card metric">
        <span>Container restarts</span><strong
          >{data.Snapshot.pods.restarts}</strong
        ><small>Across current pods</small>
      </div>
      <div class="card metric">
        <span>Nodes under pressure</span><strong>{data.PressureNodes}</strong
        ><small>Memory, disk, processes or network</small>
      </div>
    </div>
    <div class="usage-charts">
      <ClusterCapacity data={data.Cpu} /><ClusterCapacity data={data.Memory} />
      <section class="card usage-card cluster-capacity-card">
        <header>
          <h3>Disk usage</h3>
          {#if truth(data.Disk.Available)}<span class="field-help"
              >{Number(data.Disk.Percent).toFixed(1)}% used</span
            >{/if}
        </header>
        <div class="usage-reading">
          <strong
            >{#if truth(data.Disk.Available)}{data.Disk
                .Value}{:else}Unavailable{/if}</strong
          >
        </div>
        {#if truth(data.Disk.Available)}<meter
            class="cluster-capacity-meter"
            min="0"
            max="100"
            value={data.Disk.Percent}
            aria-label="Disk usage as a percentage of total filesystem capacity"
            >{Number(data.Disk.Percent).toFixed(1)}%</meter
          >{:else}<p class="field-help cluster-capacity-missing">
            Recent filesystem readings are required from every node.
          </p>{/if}
        <footer>
          <span>Total capacity</span><strong>{data.Disk.Capacity}</strong>
        </footer>
      </section>
    </div>
    <p class="field-help">
      Disk usage measures space consumed on each node’s root filesystem.
    </p>
    {#if truth(data.Snapshot.usageNotice)}<p class="field-help" role="status">
        {data.Snapshot.usageNotice}
      </p>{/if}
    <section class="card management-panel">
      <header>
        <h2>Nodes</h2>
        <p>
          Live readiness, usage, and node disk capacity. Allocatable capacity is
          the portion available to workloads, not current free space.
        </p>
      </header>
      <section>
        {#if truth(data.Nodes)}<div class="table-scroll">
            <table class="table">
              <thead
                ><tr
                  ><th>Node</th><th>Readiness</th><th>CPU</th><th>Memory</th><th
                    >Disk capacity</th
                  ><th>Pressure</th></tr
                ></thead
              ><tbody
                >{#each data.Nodes as node}<tr
                    ><td class="mono">{node.Name}</td><td
                      ><span
                        class="badge"
                        data-variant={truth(node.State === "Ready")
                          ? "secondary"
                          : "destructive"}>{node.State}</span
                      >{#if truth(node.Unschedulable)}<small
                          class="cluster-capacity-caption"
                          >Scheduling paused</small
                        >{/if}</td
                    ><td
                      >{node.Cpu}<small class="cluster-capacity-caption"
                        >of {node.CpuCapacity}</small
                      ></td
                    ><td
                      >{node.Memory}<small class="cluster-capacity-caption"
                        >of {node.MemoryCapacity}</small
                      ></td
                    ><td
                      >{node.DiskCapacity}<small
                        class="cluster-capacity-caption"
                        >{node.DiskAllocatable} allocatable</small
                      ></td
                    ><td
                      >{#if truth(node.Pressure)}<span class="cluster-pressure"
                          >{node.Pressure}</span
                        >{:else}None reported{/if}</td
                    ></tr
                  >{/each}</tbody
              >
            </table>
          </div>{:else}<p class="empty-copy">
            No nodes were reported by this cluster.
          </p>{/if}
      </section>
    </section>
    <section class="card management-panel">
      <header>
        <h2>Workload state</h2>
        <p>
          Pod phases across all namespaces, including infrastructure workloads.
        </p>
      </header>
      <section>
        <dl class="cluster-pod-states">
          <div>
            <dt>Running</dt>
            <dd>{data.Snapshot.pods.running}</dd>
          </div>
          <div>
            <dt>Pending</dt>
            <dd>{data.Snapshot.pods.pending}</dd>
          </div>
          <div>
            <dt>Failed</dt>
            <dd>{data.Snapshot.pods.failed}</dd>
          </div>
          <div>
            <dt>Completed</dt>
            <dd>{data.Snapshot.pods.succeeded}</dd>
          </div>
          <div>
            <dt>Unknown</dt>
            <dd>{data.Snapshot.pods.unknown}</dd>
          </div>
        </dl>
      </section>
    </section>
    <p class="field-help">
      Live snapshot read {timeLabel(data.Snapshot.observedAt)}
    </p>
  {/if}
</div>
