<script lang="ts">
  import { loadPanel, submitForm } from "../lib/actions";
  import { variant } from "../lib/format";
  import type { monitoringViewData } from "../lib/models";
  export let data: monitoringViewData;
</script>

<div class="section-heading">
  <div>
    <h2>{data.LogsTitle}</h2>
    <p>{data.LogsDescription}</p>
  </div>
</div>
<form
  class="log-controls"
  action={data.Endpoint}
  method="get"
  use:submitForm={{ target: "#application-logs-host" }}
>
  <input type="hidden" name="panel" value="logs" /><input
    type="hidden"
    name="organization"
    value={data.Organization}
  /><input type="hidden" name="space" value={data.Space} /><input
    type="hidden"
    name="app"
    value={data.Name}
  /><label class="sr-only" for="log-search">Search logs</label><input
    id="log-search"
    class="input"
    name="search"
    maxlength="256"
    placeholder="Filter by text, e.g. error"
  /><label class="sr-only" for="log-range">Log history</label><select
    id="log-range"
    class="select"
    name="range"
    ><option value="30m">Last 30 minutes</option><option value="1h" selected
      >Last hour</option
    ><option value="12h">Last 12 hours</option><option value="3d"
      >Last 3 days</option
    ></select
  ><label class="checkbox-label"
    ><input
      class="checkbox"
      type="checkbox"
      name="follow"
      checked
      value="on"
    />Auto-refresh</label
  ><button class="btn" type="submit">Apply filters</button><button
    class="btn"
    type="button"
    data-variant="outline"
    data-download-logs>Download</button
  >
</form>
<div
  id="application-logs-host"
  use:loadPanel={{
    url: data.Path + "&panel=logs&range=1h&follow=on",
    lazy: false,
    interval: 0,
  }}
>
  <p class="panel-loading" role="status">Loading recent output…</p>
</div>
