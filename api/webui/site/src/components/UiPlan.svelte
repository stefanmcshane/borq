<script lang="ts">
  import UiScope from "./UiScope.svelte";
  import { submitForm } from "../lib/actions";
  import { truth, yaml, variant, pageUrl } from "../lib/format";
  import type { pageData } from "../lib/models";
  export let data: pageData;
  $: application = data.Draft.name || data.Application.name;
</script>

<section class="card management-panel plan-panel">
  <header>
    <h2>Review deployment plan</h2>
    <p>
      These saved resources will be applied to {data.Plan.space.name} on {data
        .Plan.space.cluster}.
    </p>
  </header>
  <section>
    <div class="metadata">
      <span>Base revision {data.Plan.baseRevision}</span><span class="mono"
        >{data.Plan.id}</span
      >
    </div>
    <h3>Configuration changes</h3>
    <pre
      class="configuration-diff">{#if truth(data.Plan.configurationDiff)}{data
          .Plan
          .configurationDiff}{:else}No configuration changes. The plan can still repair resource drift.{/if}</pre>
    <div class="table-scroll">
      <table class="table">
        <thead><tr><th>Resource</th><th>Action</th></tr></thead><tbody
          >{#each data.Plan.resourceDiffs as resourceDiff}<tr
              ><td>{resourceDiff.kind} / {resourceDiff.name}</td><td
                ><span class="badge" data-variant="outline"
                  >{resourceDiff.action}</span
                ></td
              ></tr
            >{/each}</tbody
        >
      </table>
    </div>
    <form
      class="form-actions"
      action="/api/ui/actions"
      method="post"
      use:submitForm={{
        errorRecovery: {
          label: application ? "Review configuration" : "View applications",
          description:
            "Review the saved settings and create a new plan before deploying again.",
          href: application
            ? pageUrl(
                "application",
                data.Organization,
                data.Space.name,
                "app",
                application,
                "tab",
                "configuration",
                "draft",
                data.Plan.id,
              )
            : pageUrl("applications", data.Organization, data.Space.name),
        },
      }}
    >
      <UiScope {data} />{#if data.ApplicationTab === "configuration"}<input
          type="hidden"
          name="subtab"
          value={data.ApplicationSubtab}
        />{/if}<input type="hidden" name="action" value="apply-plan" /><input
        type="hidden"
        name="application"
        value={data.Draft.name}
      /><input type="hidden" name="plan" value={data.Plan.id} /><button
        class="btn"
        type="submit">Apply reviewed plan</button
      ><a class="btn" data-variant="ghost" href={data.ReviewReturnUrl}>Cancel</a
      >
      <details class="plan-manifests">
        <summary class="btn disclosure-button" data-variant="outline"
          >Exact manifests</summary
        >
        <pre>{yaml(data.Plan.manifests)}</pre>
      </details>
    </form>
  </section>
</section>
