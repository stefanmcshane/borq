<script lang="ts">
  import { variant } from "../lib/format";
  import type { ApiOperation } from "../lib/models";
  export let data: ApiOperation;
</script>

<section class="card management-panel">
  <header class="panel-heading">
    <div>
      <h2>Custom addon progress</h2>
      <p>{data.message}</p>
    </div>
    <span class="badge" data-variant={variant(data.status)}>{data.status}</span>
  </header>
</section>
