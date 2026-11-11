<script lang="ts">
  import UiIcon from "./UiIcon.svelte";
  import { variant } from "../lib/format";
  import type { pageData } from "../lib/models";
  export let data: pageData;
</script>

<a
  id="refresh-workspace"
  class="btn refresh-button"
  data-variant="outline"
  data-size="sm"
  href={data.Location}><UiIcon data={"refresh"} />Refresh</a
>
