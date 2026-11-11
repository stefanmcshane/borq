<script lang="ts">
  import UiIcon from "./UiIcon.svelte";
  import { truth, pageUrl, variant } from "../lib/format";
  import type { pageData } from "../lib/models";
  export let data: pageData;
</script>

<section class="card management-panel environment-group-settings">
  <header>
    <h2>Applications using this group</h2>
    <p>
      Change a group or its version from the application’s Configuration page.
      Publishing variables leaves existing deployment pins in place.
    </p>
  </header>
  <section>
    {#if truth(data.GroupApplications)}<div class="table-scroll">
        <table class="table">
          <thead
            ><tr><th>Application</th><th>Pinned version</th><th>Image</th></tr
            ></thead
          ><tbody
            >{#each data.GroupApplications as application}<tr
                class="resource-row"
                ><td
                  ><a
                    class="text-link resource-row-link"
                    href={pageUrl(
                      "application",
                      data.Organization,
                      data.Space.name,
                      "app",
                      application.name,
                    )}>{application.name}</a
                  ></td
                ><td
                  ><span class="badge" data-variant="outline"
                    >v{application.env.version}</span
                  ></td
                ><td class="mono">{application.image}</td></tr
              >{/each}</tbody
          >
        </table>
      </div>{:else}<div class="resource-empty">
        <UiIcon data={"applications"} />
        <h3>No applications are using this group</h3>
        <p>
          Select this group from an application’s environment dropdown, then
          review and deploy the change.
        </p>
        <a
          class="btn"
          data-variant="outline"
          href={pageUrl("applications", data.Organization, data.Space.name)}
          >View applications</a
        >
      </div>{/if}
  </section>
</section>
