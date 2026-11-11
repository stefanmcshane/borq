<script lang="ts">
  import type { ApiOperation } from "../lib/models";
  export let data: ApiOperation;
</script>

<section class="card management-panel">
  <header>
    <h2>Database operation</h2>
    <p>{data.status} · {data.message}</p>
  </header>
</section>
