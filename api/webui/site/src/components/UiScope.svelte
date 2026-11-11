<script lang="ts">
  import { truth } from "../lib/format";
  import type { pageData } from "../lib/models";
  export let data: pageData;
</script>

<input type="hidden" name="organization" value={data.Organization} /><input
  type="hidden"
  name="space"
  value={data.Space.name}
/><input
  type="hidden"
  name="view"
  value={truth(data.View === "custom-chart")
    ? "custom-charts"
    : truth(data.View === "application")
      ? "applications"
      : truth(data.View === "environment-group")
        ? "environment"
        : data.View}
/>{#if data.View === "application"}<input
    type="hidden"
    name="tab"
    value={data.ApplicationTab}
  />{#if data.ApplicationTab !== "configuration"}<input
      type="hidden"
      name="subtab"
      value={data.ApplicationSubtab}
    />{/if}{:else if data.View === "postgres-detail"}<input
    type="hidden"
    name="tab"
    value={data.Postgres.Tab}
  /><input type="hidden" name="subtab" value={data.Postgres.Subtab} />{/if}
