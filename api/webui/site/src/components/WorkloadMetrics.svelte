<script lang="ts">
  import { loadPanel, submitForm } from "../lib/actions";
  import { variant } from "../lib/format";
  import type { monitoringViewData } from "../lib/models";
  export let data: monitoringViewData;
</script>

<section class="app-monitoring">
  <div class="section-heading">
    <div>
      <h2>Resource usage</h2>
      <p>
        {data.Endpoint === "/api/ui/space"
          ? "Across collected containers in this space"
          : "Across selected pods"} · collected automatically
      </p>
    </div>
    <form
      class="range-controls"
      action={data.Endpoint}
      method="get"
      use:submitForm={{ target: "#application-metrics-host" }}
    >
      <input
        type="hidden"
        name="organization"
        value={data.Organization}
      /><input type="hidden" name="space" value={data.Space} /><input
        type="hidden"
        name="app"
        value={data.Name}
      /><input type="hidden" name="panel" value="metrics" /><button
        class="btn"
        data-variant="outline"
        name="range"
        value="30m"
        type="submit">30 minutes</button
      ><button
        class="btn"
        data-variant="outline"
        name="range"
        value="1h"
        type="submit">1 hour</button
      ><button
        class="btn"
        data-variant="outline"
        name="range"
        value="12h"
        type="submit">12 hours</button
      ><button
        class="btn"
        data-variant="outline"
        name="range"
        value="3d"
        type="submit">3 days</button
      >
    </form>
  </div>
  <div
    id="application-metrics-host"
    use:loadPanel={{
      url: data.Path + "&panel=metrics&range=1h",
      lazy: false,
      interval: 0,
    }}
  >
    <p class="panel-loading" role="status">Loading CPU and memory history…</p>
  </div>
</section>
