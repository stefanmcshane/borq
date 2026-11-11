<script lang="ts">
  import type { ApiChartDefaults } from "../lib/models";
  export let data: ApiChartDefaults;
</script>

<div class="chart-defaults-heading">
  <strong
    >{data.chart.name} <span class="mono">{data.chart.version}</span></strong
  >
</div>
<pre
  class="mono"
  tabindex="-1"
  aria-label="Default values YAML">{data.values}</pre>
