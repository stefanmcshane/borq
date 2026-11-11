<script lang="ts">
  import { onDestroy, tick } from "svelte";
  import { request, type Envelope } from "../lib/api";
  import { registerPanel } from "../lib/actions";
  import { terminal } from "../lib/format";
  import Workspace from "./Workspace.svelte";
  import ApplicationEditor from "./ApplicationEditor.svelte";
  import EnvironmentEditor from "./EnvironmentEditor.svelte";
  import ManagementOperation from "./ManagementOperation.svelte";
  import UiOperation from "./UiOperation.svelte";
  import PostgresOperation from "./PostgresOperation.svelte";
  import CustomChartOperation from "./CustomChartOperation.svelte";
  import UiComponents from "./UiComponents.svelte";
  import ClusterSetup from "./ClusterSetup.svelte";
  import ClusterMetrics from "./ClusterMetrics.svelte";
  import CustomChartDefaults from "./CustomChartDefaults.svelte";
  import ApplicationStatus from "./ApplicationStatus.svelte";
  import ApplicationRuntime from "./ApplicationRuntime.svelte";
  import ApplicationMetrics from "./ApplicationMetrics.svelte";
  import ApplicationLogs from "./ApplicationLogs.svelte";
  import ApplicationDeployments from "./ApplicationDeployments.svelte";
  import ApplicationEvents from "./ApplicationEvents.svelte";
  import PostgresMetrics from "./PostgresMetrics.svelte";
  import PostgresLogs from "./PostgresLogs.svelte";

  export let initial: Envelope;
  export let url = "";
  export let operationScope = "";
  export let refreshWorkspaceOnComplete = false;
  export let recoveryActions: { label: string; href: string }[] = [];
  let current = initial;
  let previousInitial = initial;
  let root: HTMLDivElement;
  let error = "";
  let timer: ReturnType<typeof setTimeout> | undefined;
  let pending: AbortController | undefined;
  let stopped = false;
  let generation = 0;

  function schedule() {
    clearTimeout(timer);
    const next = refresh(current);
    if (!stopped && next && next.interval)
      timer = setTimeout(() => load(next.url), next.interval);
  }
  function refresh(
    value: Envelope,
  ): { url: string; interval: number } | undefined {
    switch (value.component) {
      case "ui-operation":
      case "postgres-operation":
      case "custom-chart-operation": {
        const route =
          value.component === "ui-operation"
            ? "operations"
            : value.component === "postgres-operation"
              ? "postgres/operations"
              : "custom-charts/operations";
        return terminal(value.data.status)
          ? undefined
          : {
              url:
                "/api/ui/" +
                route +
                "/" +
                encodeURIComponent(value.data.id) +
                (value.component === "ui-operation" ? operationScope : ""),
              interval: 2000,
            };
      }
      case "management-operation":
        return terminal(value.data.status)
          ? undefined
          : {
              url:
                "/api/ui/management/operation?managementOperation=" +
                encodeURIComponent(value.data.id),
              interval: 3000,
            };
      case "application-runtime":
        return { url: value.data.Location, interval: 15000 };
      case "application-status":
        return url ? { url, interval: 30000 } : undefined;
      case "application-deployments":
      case "application-events":
        return { url: value.data.Location, interval: 15000 };
      case "application-logs":
        return {
          url: value.data.Location,
          interval: value.data.Follow ? 5000 : 0,
        };
      case "application-metrics":
        return { url: value.data.Location, interval: 60000 };
      case "ui-components":
        return { url: value.data.Location, interval: 10000 };
      case "cluster-metrics":
      case "postgres-metrics":
        return { url: value.data.Location, interval: 30000 };
      case "postgres-logs":
        return { url: value.data.Location, interval: 15000 };
      default:
        return undefined;
    }
  }
  function operationStatus(value: Envelope) {
    return value.component === "ui-operation" ||
      value.component === "management-operation" ||
      value.component === "postgres-operation" ||
      value.component === "custom-chart-operation"
      ? value.data.status
      : undefined;
  }
  export async function replace(next: Envelope) {
    pending?.abort();
    generation++;
    const expanded = Array.from(
      root.querySelectorAll<HTMLDetailsElement>("details[open]"),
      (element) => element.id,
    ).filter(Boolean);
    const output = root.querySelector<HTMLElement>("[data-log-output]");
    const position = output && {
      top: output.scrollTop,
      bottom: output.scrollHeight - output.clientHeight - output.scrollTop < 40,
    };
    const before = operationStatus(current);
    current = next;
    error = "";
    schedule();
    await tick();
    for (const id of expanded) {
      const details = document.getElementById(id);
      if (details instanceof HTMLDetailsElement) details.open = true;
    }
    const updated = root.querySelector<HTMLElement>("[data-log-output]");
    if (updated)
      updated.scrollTop =
        position && !position.bottom ? position.top : updated.scrollHeight;
    document.dispatchEvent(new CustomEvent("borq:render"));
    const after = operationStatus(next);
    if (
      before &&
      !terminal(before) &&
      after &&
      terminal(after) &&
      (!root.closest(".inline-operation") || refreshWorkspaceOnComplete)
    ) {
      document.dispatchEvent(new CustomEvent("borq:refresh"));
    }
  }
  async function load(address: string) {
    pending?.abort();
    const controller = new AbortController();
    pending = controller;
    const version = ++generation;
    try {
      const result = await request(address, { signal: controller.signal });
      if (!stopped && version === generation) await replace(result);
    } catch (cause) {
      if (!controller.signal.aborted) {
        error =
          cause instanceof Error
            ? cause.message
            : "Unable to refresh this panel.";
        schedule();
      }
    }
  }
  $: if (root) schedule();
  $: if (initial !== previousInitial) {
    previousInitial = initial;
    if (root) void replace(initial);
    else current = initial;
  }
  onDestroy(() => {
    stopped = true;
    clearTimeout(timer);
    pending?.abort();
  });
</script>

<div class="panel-host" bind:this={root} use:registerPanel={replace}>
  {#if current.component === "workspace"}
    <Workspace data={current.data} />
  {:else if current.component === "application-editor"}
    <ApplicationEditor data={current.data} />
  {:else if current.component === "environment-editor"}
    <EnvironmentEditor data={current.data} />
  {:else if current.component === "ui-operation"}
    <UiOperation data={current.data} {recoveryActions} scope={operationScope} />
  {:else if current.component === "management-operation"}
    <ManagementOperation data={current.data} />
  {:else if current.component === "postgres-operation"}
    <PostgresOperation data={current.data} />
  {:else if current.component === "custom-chart-operation"}
    <CustomChartOperation data={current.data} />
  {:else if current.component === "ui-components"}
    <UiComponents data={current.data} />
  {:else if current.component === "cluster-setup"}
    <ClusterSetup data={current.data} />
  {:else if current.component === "cluster-metrics"}
    <ClusterMetrics data={current.data} />
  {:else if current.component === "custom-chart-defaults"}
    <CustomChartDefaults data={current.data} />
  {:else if current.component === "application-status"}
    <ApplicationStatus data={current.data} />
  {:else if current.component === "application-runtime"}
    <ApplicationRuntime data={current.data} />
  {:else if current.component === "application-metrics"}
    <ApplicationMetrics data={current.data} />
  {:else if current.component === "application-logs"}
    <ApplicationLogs data={current.data} />
  {:else if current.component === "application-deployments"}
    <ApplicationDeployments data={current.data} />
  {:else if current.component === "application-events"}
    <ApplicationEvents data={current.data} />
  {:else if current.component === "postgres-metrics"}
    <PostgresMetrics data={current.data} />
  {:else if current.component === "postgres-logs"}
    <PostgresLogs data={current.data} />
  {/if}
  {#if error}<div class="alert request-error" role="alert">
      <p>{error}</p>
      <button
        class="btn"
        data-variant="outline"
        type="button"
        onclick={() => {
          const address = refresh(current)?.url || url;
          if (address) void load(address);
          else document.dispatchEvent(new CustomEvent("borq:refresh"));
        }}>Try again</button
      >
    </div>{/if}
</div>

<style>
  .panel-host {
    display: contents;
  }
</style>
