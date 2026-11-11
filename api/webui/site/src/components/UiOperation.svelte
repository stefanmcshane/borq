<script lang="ts">
  import { submitForm, requestButton } from "../lib/actions";
  import { truth, terminal, variant } from "../lib/format";
  import type { ApiOperation } from "../lib/models";
  export let data: ApiOperation;
  export let scope = "";
  export let recoveryActions: { label: string; href: string }[] = [];
</script>

<section class="card management-panel operation-panel">
  <header class="panel-heading">
    <div>
      <h2>
        {data.phase === "deleting"
          ? "Application deletion"
          : "Deployment progress"}
      </h2>
      <p>{data.message}</p>
    </div>
    <span class="badge" data-variant={variant(data.status)}>{data.status}</span>
  </header>
  <section>
    {#if data.status === "failed"}
      <p class="field-help">
        {#if data.phase === "deleting"}
          Check the failure details, then open the application’s Configuration
          page to retry deletion.
        {:else}
          Check the failure details and update the configuration before creating
          a new deployment.
        {/if}
      </p>
      <div class="form-actions">
        {#each recoveryActions as action}
          <a class="btn" data-variant="outline" href={action.href}
            >{action.label}</a
          >
        {:else}
          <button
            class="btn"
            data-variant="outline"
            type="button"
            use:requestButton={{
              url: "/api/ui/operations/" + encodeURIComponent(data.id) + scope,
              method: "GET",
              target: "closest .operation-panel",
            }}
            >{data.phase === "deleting"
              ? "Refresh deletion status"
              : "Refresh deployment status"}</button
          >
        {/each}
      </div>
    {/if}
    {#if !terminal(data.status) && data.status !== "canceling" && data.phase !== "deleting"}<form
        action={"/api/ui/operations/" +
          encodeURIComponent(data.id) +
          "/cancel" +
          scope}
        method="post"
        use:submitForm={{ target: "closest .operation-panel" }}
      >
        <button class="btn" data-variant="outline" type="submit"
          >Cancel deployment</button
        >
        <p class="field-help">
          Stops this deployment attempt and retains accepted settings. Resources
          already applied remain in the cluster.
        </p>
      </form>{/if}
    <div class="metadata">
      <span>Phase: {data.phase}</span><span class="mono">{data.id}</span>
    </div>
    {#if truth(data.resources)}<div class="table-scroll">
        <table class="table">
          <thead
            ><tr><th>Resource</th><th>Status</th><th>Details</th></tr></thead
          ><tbody
            >{#each data.resources as resource}<tr
                ><td>{resource.kind} / {resource.name}</td><td
                  >{resource.state}</td
                ><td>{resource.message}</td></tr
              >{/each}</tbody
          >
        </table>
      </div>{/if}
  </section>
</section>
