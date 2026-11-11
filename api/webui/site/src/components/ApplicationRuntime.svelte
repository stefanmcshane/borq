<script lang="ts">
  import ApplicationStatus from "./ApplicationStatus.svelte";
  import { truth } from "../lib/format";
  import type { applicationPanelData } from "../lib/models";
  export let data: applicationPanelData;
</script>

<section class="card management-panel runtime-panel">
  <section>
    <div class="runtime-summary">
      <div>
        <ApplicationStatus {data} />
        <p class="field-help">
          {#if truth(data.Problem)}{data.Problem}{:else}{data.Details
              .message}{/if}
        </p>
      </div>
      <span class="field-help">Live cluster health · refreshes every 15s</span>
    </div>
    {#if truth(data.Details.resources)}<details class="runtime-resources">
        <summary>Resource details</summary>
        <div class="table-scroll">
          <table class="table">
            <thead
              ><tr><th>Resource</th><th>State</th><th>Details</th></tr></thead
            ><tbody
              >{#each data.Details.resources as resource}<tr
                  ><td>{resource.kind} / {resource.name}</td><td
                    >{resource.state}</td
                  ><td>{resource.message}</td></tr
                >{/each}</tbody
            >
          </table>
        </div>
      </details>{/if}
  </section>
</section>
