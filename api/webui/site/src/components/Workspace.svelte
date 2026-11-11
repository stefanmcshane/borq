<script lang="ts">
  import UiOrganizationDialog from "./UiOrganizationDialog.svelte";
  import ViewApplication from "./ViewApplication.svelte";
  import ViewApplications from "./ViewApplications.svelte";
  import ViewInfrastructure from "./ViewInfrastructure.svelte";
  import ViewCustomChart from "./ViewCustomChart.svelte";
  import ViewCustomCharts from "./ViewCustomCharts.svelte";
  import ViewEnvironment from "./ViewEnvironment.svelte";
  import ViewEnvironmentGroup from "./ViewEnvironmentGroup.svelte";
  import ViewOverview from "./ViewOverview.svelte";
  import ViewOrganizationAuthentication from "./ViewOrganizationAuthentication.svelte";
  import ViewOrganizationMembers from "./ViewOrganizationMembers.svelte";
  import ViewSpacePocketId from "./ViewSpacePocketId.svelte";
  import ViewPostgres from "./ViewPostgres.svelte";
  import ViewPostgresDetail from "./ViewPostgresDetail.svelte";
  import ViewSpaces from "./ViewSpaces.svelte";
  import { truth } from "../lib/format";
  import type { pageData } from "../lib/models";
  export let data: pageData;
</script>

<main id="workspace" tabindex="-1">
  <div class="sr-only"><h1>{data.Title}</h1></div>
  <div id="notice" aria-live="polite">
    {#if truth(data.Notice)}<p class="alert notice-success" role="status">
        {data.Notice}
      </p>{/if}
  </div>
  {#if data.AwaitingRole}
    <section class="empty workspace-empty" aria-label="Organization access">
      <h2>Waiting for a role</h2>
      <p>
        You have signed in to {data.Organization}. Ask an organization
        administrator to assign you a role, then refresh this page.
      </p>
    </section>
  {:else if data.View === "infrastructure"}<ViewInfrastructure {data} />
  {:else if !truth(data.Organization)}
    <section class="empty workspace-empty onboarding">
      <header>
        <h2>Create your first organization</h2>
        <p>
          Organizations bring your clusters, spaces, and applications together.
          Open the Org menu above to create one.
        </p>
      </header>
    </section>
  {:else if data.View === "overview"}<ViewOverview {data} />
  {:else if data.View === "organization-authentication"}<ViewOrganizationAuthentication
      {data}
    />
  {:else if data.View === "organization-members"}<ViewOrganizationMembers
      {data}
    />
  {:else if data.View === "applications"}<ViewApplications {data} />
  {:else if data.View === "application"}<ViewApplication {data} />
  {:else if data.View === "space-pocketid"}<ViewSpacePocketId {data} />
  {:else if data.View === "postgres"}<ViewPostgres {data} />
  {:else if data.View === "postgres-detail"}<ViewPostgresDetail {data} />
  {:else if data.View === "custom-charts"}<ViewCustomCharts {data} />
  {:else if data.View === "custom-chart"}<ViewCustomChart {data} />
  {:else if data.View === "spaces"}<ViewSpaces {data} />
  {:else if data.View === "environment"}<ViewEnvironment {data} />
  {:else if data.View === "environment-group"}<ViewEnvironmentGroup {data} />
  {/if}
  <UiOrganizationDialog {data} />
</main>
