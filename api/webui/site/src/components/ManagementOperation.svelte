<script lang="ts">
  import type { ApiOperation } from "../lib/models";
  export let data: ApiOperation;
</script>

<section class="card management-panel infrastructure-operation">
  <header>
    <h2>Configuration apply</h2>
    <p role={data.status === "failed" ? "alert" : "status"}>
      {data.status} · {data.message}
    </p>
  </header>
</section>
