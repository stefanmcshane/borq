<script lang="ts">
  import { pageUrl, variant } from "../lib/format";
  import type { pageData } from "../lib/models";
  export let data: pageData;
</script>

<section class="alert">
  <h2>Create a space to use secrets</h2>
  <p>
    Environment versions are stored separately from application configuration
    and selected during deployment planning.
  </p>
  <a
    class="btn"
    data-variant="outline"
    href={pageUrl("spaces", data.Organization, data.Space.name)}>Create space</a
  >
</section>
