<script lang="ts">
  import WorkloadEvent from "./WorkloadEvent.svelte";
  import { submitForm } from "../lib/actions";
  import { truth, variant } from "../lib/format";
  import type { applicationPanelData } from "../lib/models";
  export let data: applicationPanelData;
</script>

<section id="application-events" class="card management-panel">
  <header class="panel-heading">
    <div>
      <h2>Events</h2>
      <p>Installation, upgrade, and pod activity · times shown in UTC.</p>
    </div>
    <form
      class="event-history-controls"
      action={data.Endpoint}
      method="get"
      use:submitForm={{ target: "#application-events" }}
    >
      <input
        type="hidden"
        name="organization"
        value={data.Organization}
      /><input type="hidden" name="space" value={data.Space} /><input
        type="hidden"
        name="app"
        value={data.Application}
      /><input type="hidden" name="panel" value="events" /><label
        class="sr-only"
        for="event-range">Event history</label
      ><select id="event-range" class="select" name="range"
        ><option value="30m" selected={truth(data.Range === "30m")}
          >Last 30 minutes</option
        ><option value="1h" selected={truth(data.Range === "1h")}
          >Last hour</option
        ><option value="12h" selected={truth(data.Range === "12h")}
          >Last 12 hours</option
        ><option value="3d" selected={truth(data.Range === "3d")}
          >Last 3 days</option
        ></select
      ><button class="btn" data-variant="outline" type="submit">Update</button>
    </form>
  </header>
  <section>
    {#if truth(data.Problem)}<p class="empty-copy">
        {data.Problem}
      </p>{:else if truth(data.Rows)}<div
        class="log-output workload-event-output"
        tabindex="0"
        role="textbox"
        aria-readonly="true"
        aria-multiline="true"
        aria-label="Addon events"
      >
        {#each data.Rows as row}<WorkloadEvent data={row} />{/each}
      </div>{:else}<p class="empty-copy">
        No events in this time range.
      </p>{/if}{#if truth(data.History.eventsNotice)}<p
        class="field-help"
        role="status"
      >
        {data.History.eventsNotice}
      </p>{/if}
  </section>
</section>
