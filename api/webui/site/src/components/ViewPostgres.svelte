<script lang="ts">
  import { infrastructureUrl } from "../lib/infrastructure";
  import ManagedAddonNavigation from "./ManagedAddonNavigation.svelte";
  import UiScope from "./UiScope.svelte";
  import UiIcon from "./UiIcon.svelte";
  import PostgresEditor from "./PostgresEditor.svelte";
  import { submitForm } from "../lib/actions";
  import ComponentHost from "./ComponentHost.svelte";
  import { truth, pageUrl } from "../lib/format";
  import type { pageData } from "../lib/models";
  export let data: pageData;
  let configurationValid = true;
</script>

<ManagedAddonNavigation {data} />
<div class="section-heading">
  <div>
    <h2>Managed PostgreSQL</h2>
    <p>
      Isolated databases with automatic full backups, continuous WAL and weekly
      recovery verification.
    </p>
    <p class="field-help">
      Configure backup archive storage in <a
        class="text-link"
        href={infrastructureUrl({
          Tab: "settings",
          ComponentScope: "postgres",
        })}>Settings</a
      > before creating a database.
    </p>
  </div>
  <button class="btn" type="button" data-dialog-open="postgres-create"
    >Create database</button
  >
</div>
{#if truth(data.Postgres.Operation.id)}<ComponentHost
    initial={{ component: "postgres-operation", data: data.Postgres.Operation }}
  />{/if}
{#if truth(data.Space.id)}<section class="card management-panel">
    <section>
      {#if truth(data.Postgres.Databases)}<div class="table-scroll">
          <table class="table">
            <thead
              ><tr><th>Database</th><th>State</th><th>Backup protection</th></tr
              ></thead
            ><tbody
              >{#each data.Postgres.Databases as database}<tr
                  ><td
                    ><a
                      class="text-link"
                      href={pageUrl(
                        "postgres-detail",
                        data.Organization,
                        data.Space.name,
                        "postgres",
                        database.name,
                      )}>{database.name}</a
                    ></td
                  ><td>{database.state}</td><td>{database.protection.status}</td
                  ></tr
                >{/each}</tbody
            >
          </table>
        </div>{:else}<div class="resource-empty">
          <UiIcon data="database" />
          <h3>No databases yet</h3>
          <p>Create a managed PostgreSQL database using only a name.</p>
          <button class="btn" type="button" data-dialog-open="postgres-create"
            >Create database</button
          >
        </div>{/if}
    </section>
  </section>
  <dialog
    id="postgres-create"
    class="workspace-dialog"
    aria-labelledby="postgres-create-title"
  >
    <header class="dialog-heading">
      <div>
        <h2 id="postgres-create-title">Create PostgreSQL database</h2>
        <p>
          One instance and 1Gi storage by default. Automatic protection starts
          with the database.
        </p>
      </div>
      <button class="btn" data-variant="ghost" type="button" data-dialog-close
        >Close</button
      >
    </header>
    <form
      class="form-grid"
      data-postgres-editor
      action="/api/ui/actions"
      method="post"
      use:submitForm={{}}
    >
      <UiScope {data} /><input
        type="hidden"
        name="action"
        value="put-postgres"
      />
      {#key data.Organization + "/" + data.Space.name}
        <PostgresEditor
          id="postgres-create"
          create
          bind:valid={configurationValid}
        />
      {/key}
      <div class="form-actions">
        <button class="btn" type="submit" disabled={!configurationValid}
          >Create database</button
        >
      </div>
    </form>
  </dialog>
{:else}<div class="resource-empty">
    <UiIcon data="database" />
    <h3>Create a space first</h3>
    <p>Create or select a space before adding a database.</p>
    <a class="btn" href={pageUrl("spaces", data.Organization, "")}
      >Create a space</a
    >
  </div>{/if}
