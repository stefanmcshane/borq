<script lang="ts">
  import UiScope from "./UiScope.svelte";
  import PostgresEditor from "./PostgresEditor.svelte";
  import { loadPanel, submitForm } from "../lib/actions";
  import ComponentHost from "./ComponentHost.svelte";
  import {
    truth,
    pageUrl,
    yaml,
    timeLabel,
    bytesLabel,
    variant,
  } from "../lib/format";
  import type { pageData } from "../lib/models";
  export let data: pageData;
  let configurationValid = true;
  $: database = data.Postgres.Database;
</script>

<div class="section-heading">
  <div>
    <h2>{database.name}</h2>
    <p>
      Managed PostgreSQL · {database.namespace} ·
      <span class="badge" data-variant={variant(database.state)}
        >{database.state}</span
      >
    </p>
  </div>
  <a
    class="btn"
    data-variant="outline"
    href={pageUrl("postgres", data.Organization, data.Space.name)}
    >All databases</a
  >
</div>
<p class="field-help">{database.message}</p>
{#if truth(data.Postgres.Operation.id)}<ComponentHost
    initial={{ component: "postgres-operation", data: data.Postgres.Operation }}
  />{/if}
<nav class="application-tabs" aria-label="Database sections">
  <a
    href={pageUrl(
      "postgres-detail",
      data.Organization,
      data.Space.name,
      "postgres",
      database.name,
      "tab",
      "overview",
      "range",
      data.Postgres.Range,
      "search",
      data.Postgres.Search,
      "end",
      data.Postgres.End,
    )}
    aria-current={truth(data.Postgres.Tab === "overview") ? "page" : undefined}
    >Overview</a
  >
  <a
    href={pageUrl(
      "postgres-detail",
      data.Organization,
      data.Space.name,
      "postgres",
      database.name,
      "tab",
      "backups",
      "range",
      data.Postgres.Range,
      "search",
      data.Postgres.Search,
      "end",
      data.Postgres.End,
    )}
    aria-current={truth(data.Postgres.Tab === "backups") ? "page" : undefined}
    >Backups</a
  >
  <a
    href={pageUrl(
      "postgres-detail",
      data.Organization,
      data.Space.name,
      "postgres",
      database.name,
      "tab",
      "configuration",
      "range",
      data.Postgres.Range,
      "search",
      data.Postgres.Search,
      "end",
      data.Postgres.End,
    )}
    aria-current={truth(data.Postgres.Tab === "configuration")
      ? "page"
      : undefined}>Configuration</a
  >
  <a
    href={pageUrl(
      "postgres-detail",
      data.Organization,
      data.Space.name,
      "postgres",
      database.name,
      "tab",
      "observability",
      "range",
      data.Postgres.Range,
      "search",
      data.Postgres.Search,
      "end",
      data.Postgres.End,
    )}
    aria-current={truth(data.Postgres.Tab === "observability")
      ? "page"
      : undefined}>Observability</a
  >
</nav>
{#if data.Postgres.Tab === "overview"}
  {#if truth(database.environmentGroup)}<section class="card management-panel">
      <header>
        <h2>Application connection</h2>
        <p>
          Connection variables refresh automatically in this managed environment
          group.
        </p>
      </header>
      <section>
        <p class="app-image">
          <a
            class="text-link"
            href={pageUrl(
              "environment-group",
              data.Organization,
              data.Space.name,
              "group",
              database.environmentGroup,
            )}>{database.environmentGroup}</a
          >
        </p>
        <p class="field-help">
          Select this group in your application’s Configuration page using the
          Secret group selector, then review and deploy the change.
          Applications keep their pinned version until you deploy a new plan.
        </p>
      </section>
    </section>{/if}
  <section class="card management-panel">
    <header>
      <h2>Protection and recovery</h2>
      <p>{database.protection.message}</p>
    </header>
    <section>
      <dl class="release-metadata">
        <div>
          <dt>Backup protection</dt>
          <dd>{database.protection.status}</dd>
        </div>
        <div>
          <dt>Recovery window</dt>
          <dd>{database.protection.recoveryWindowDays} days</dd>
        </div>
        <div>
          <dt>Recovery verification</dt>
          <dd>
            {database.restoreCheck
              .status}{#if truth(database.restoreCheck.durationSeconds)}{@const value1 =
                database.restoreCheck.durationSeconds} · {value1} seconds{/if}
          </dd>
        </div>
        <div>
          <dt>Check cleanup</dt>
          <dd>
            {#if truth(database.restoreCheck.cleanup)}{@const value3 =
                database.restoreCheck.cleanup}{value3}{:else}No completed check{/if}
          </dd>
        </div>
      </dl>
      {#if truth(database.restoreCheck.message)}{@const value4 =
          database.restoreCheck.message}
        <p class="field-help">{value4}</p>{/if}
      <p class="field-help">
        Connection credentials are available through <code
          >borq am postgres env {database.name} --organization {data.Organization}
          --space {data.Space.name}</code
        >.
      </p>
    </section>
  </section>
{:else if data.Postgres.Tab === "backups"}
  <section class="card management-panel">
    <header>
      <h2>Restore from a backup</h2>
      <p>
        Restore an approved completed backup to a new database. Availability is
        checked again before recovery starts.
      </p>
    </header>
    <section>
      {#if truth(data.Postgres.BackupProblem)}<p
          class="field-help"
          role="status"
        >
          {data.Postgres.BackupProblem}
        </p>{:else if truth(data.Postgres.Backups)}<form
          class="form-grid"
          action="/api/ui/actions"
          method="post"
          use:submitForm={{}}
        >
          <UiScope {data} /><input
            type="hidden"
            name="action"
            value="restore-postgres"
          /><label
            >Backup<select class="select" name="backup" required
              ><option value="">Select an approved backup</option
              >{#each data.Postgres.Backups as backup}{#if truth(backup.eligible)}<option
                    value={backup.id}
                    >{backup.sourceName} · {timeLabel(backup.completedAt)} · PostgreSQL
                    {backup.majorVersion} · {bytesLabel(
                      backup.sizeBytes,
                    )}</option
                  >{/if}{/each}</select
            ></label
          ><label
            >New database name<input
              class="input"
              name="name"
              data-postgres-name
              autocapitalize="none"
              spellcheck="false"
              required
              maxlength="47"
              pattern={"[a-z0-9]([a-z0-9\\-]*[a-z0-9])?"}
              title="Use 1 to 47 lowercase letters, digits or hyphens. Start and end with a letter or digit."
              placeholder="database-restored"
            /></label
          >
          <p class="field-help">
            Use 1 to 47 lowercase letters, digits or hyphens. Start and end with
            a letter or digit.
          </p>
          <details>
            <summary>Point-in-time recovery</summary><label
              >Recovery time in UTC<input
                class="input"
                name="recoveryTarget"
                placeholder="2026-10-24T12:00:00Z"
              /></label
            >
            <p class="field-help">
              Leave empty to recover the latest archived transactions. A specified
              time must follow the selected backup; PostgreSQL verifies that
              the archived transaction log reaches it.
            </p>
          </details>
          <button class="btn" type="submit">Restore new database</button>
        </form>
        <details>
          <summary>Backup catalogue and recovery bounds</summary>
          <div class="table-scroll">
            <table class="table">
              <thead
                ><tr
                  ><th>Completed</th><th>Recovery start</th><th>Confirmed through</th
                  ><th>Eligibility</th></tr
                ></thead
              ><tbody
                >{#each data.Postgres.Backups as backup}<tr
                    ><td>{timeLabel(backup.completedAt)}</td><td
                      >{timeLabel(backup.recoveryStart)}</td
                    ><td>{timeLabel(backup.recoveryEnd)}</td><td
                      >{#if truth(backup.eligible)}Approved{:else}{backup.reason}{/if}</td
                    ></tr
                  >{/each}</tbody
              >
            </table>
          </div>
        </details>{:else}<p class="empty-copy">
          No completed backups are available yet. The initial full backup runs
          automatically.
        </p>{/if}
    </section>
  </section>
{:else if data.Postgres.Tab === "configuration"}
  <section class="card management-panel">
    <header><h2>Database settings</h2></header>
    <section>
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
        /><input type="hidden" name="apply" value="true" /><input
          type="hidden"
          name="name"
          value={database.name}
        />
        {#key data.Organization + "/" + data.Space.name + "/" + database.name}
          <PostgresEditor
            bind:valid={configurationValid}
            id="postgres-edit"
            configuration={yaml(database.specification)}
          />
        {/key}
        <button class="btn" type="submit" disabled={!configurationValid}
          >Apply settings</button
        >
      </form>
      <form
        action="/api/ui/actions"
        method="post"
        use:submitForm={{
          confirm:
            "Delete this database and its volumes? Its backup archive will be preserved.",
        }}
      >
        <UiScope {data} /><input
          type="hidden"
          name="action"
          value="delete-postgres"
        /><input type="hidden" name="name" value={database.name} /><button
          class="btn"
          data-variant="destructive"
          type="submit">Delete database</button
        >
      </form>
    </section>
  </section>
{:else if data.Postgres.Tab === "observability"}
  <nav class="application-subtabs" aria-label="Database observability views">
    <a
      href={pageUrl(
        "postgres-detail",
        data.Organization,
        data.Space.name,
        "postgres",
        database.name,
        "tab",
        "observability",
        "subtab",
        "metrics",
        "range",
        data.Postgres.Range,
        "search",
        data.Postgres.Search,
        "end",
        data.Postgres.End,
      )}
      aria-current={truth(data.Postgres.Subtab === "metrics")
        ? "page"
        : undefined}>Metrics</a
    ><a
      href={pageUrl(
        "postgres-detail",
        data.Organization,
        data.Space.name,
        "postgres",
        database.name,
        "tab",
        "observability",
        "subtab",
        "logs",
        "range",
        data.Postgres.Range,
        "search",
        data.Postgres.Search,
        "end",
        data.Postgres.End,
      )}
      aria-current={truth(data.Postgres.Subtab === "logs") ? "page" : undefined}
      >Logs</a
    >
  </nav>
  {#if data.Postgres.Subtab === "metrics"}
    <div
      use:loadPanel={{
        url: data.Postgres.Path + "&panel=metrics",
        lazy: false,
        interval: 0,
      }}
    >
      <p class="panel-loading" role="status">
        Reading CPU, memory, disk and query activity…
      </p>
    </div>
  {:else}
    <section class="card management-panel">
      <header>
        <h2>Database logs</h2>
        <p>
          Latest 200 lines from the selected database. Output refreshes every 15
          seconds.
        </p>
      </header>
      <section>
        <form
          class="form-actions"
          action="/api/ui/postgres"
          method="get"
          use:submitForm={{ target: "#postgres-logs" }}
        >
          <input
            type="hidden"
            name="organization"
            value={data.Organization}
          /><input type="hidden" name="space" value={data.Space.name} /><input
            type="hidden"
            name="postgres"
            value={database.name}
          /><input type="hidden" name="panel" value="logs" /><input
            type="hidden"
            name="end"
            value={data.Postgres.End}
          /><label
            >Search<input
              class="input"
              name="search"
              value={data.Postgres.Search}
              maxlength="256"
              placeholder="Literal text"
            /></label
          ><label
            >History<select class="select" name="range"
              ><option
                value="30m"
                selected={truth(data.Postgres.Range === "30m")}
                >30 minutes</option
              ><option value="1h" selected={truth(data.Postgres.Range === "1h")}
                >1 hour</option
              ><option
                value="12h"
                selected={truth(data.Postgres.Range === "12h")}>12 hours</option
              ><option value="3d" selected={truth(data.Postgres.Range === "3d")}
                >3 days</option
              ></select
            ></label
          ><button class="btn" type="submit">Filter logs</button>
        </form>
        <div
          id="postgres-logs"
          use:loadPanel={{
            url: data.Postgres.Path + "&panel=logs",
            lazy: false,
            interval: 0,
          }}
        >
          <p class="panel-loading" role="status">Reading bounded output…</p>
        </div>
      </section>
    </section>
  {/if}
{/if}
