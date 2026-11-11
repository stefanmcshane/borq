<script lang="ts">
  import { pageUrl } from "../lib/format";
  import type { pageData } from "../lib/models";
  export let data: pageData;
</script>

<nav class="application-tabs" aria-label="Managed addon type">
  <a
    href={pageUrl("postgres", data.Organization, data.Space.name)}
    aria-current={data.View === "postgres" ? "page" : undefined}>PostgreSQL</a
  >
  <a
    href={pageUrl("space-pocketid", data.Organization, data.Space.name)}
    aria-current={data.View === "space-pocketid" ? "page" : undefined}
    >Pocket ID</a
  >
</nav>
