<script lang="ts">
  import { truth, pageUrl, variant } from "../lib/format";
  import type { pageData } from "../lib/models";
  export let data: pageData;
</script>

<section class="card management-panel environment-group-settings">
  <header>
    <h2>Published versions</h2>
    <p>
      Each version is immutable. Open a version to inspect its variables{#if !truth(data.Group.managedBy)}
        or publish those values as a new version{/if}.
    </p>
  </header>
  <section>
    <div class="table-scroll">
      <table class="table">
        <thead><tr><th>Version</th><th>Status</th><th></th></tr></thead><tbody
          >{#each data.GroupVersions as version}<tr class="resource-row"
              ><td class="mono">v{version}</td><td
                >{#if version === data.GroupLatestVersion}<span
                    class="badge"
                    data-variant="secondary">Latest</span
                  >{:else}<span class="field-help">Previous</span>{/if}</td
              ><td
                ><a
                  class="text-link resource-row-link"
                  href={pageUrl(
                    "environment-group",
                    data.Organization,
                    data.Space.name,
                    "group",
                    data.Group.name,
                    "version",
                    String(version),
                  )}>View variables</a
                ></td
              ></tr
            >{/each}</tbody
        >
      </table>
    </div>
  </section>
</section>
