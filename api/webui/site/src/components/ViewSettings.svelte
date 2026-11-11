<script lang="ts">
  import InfrastructureSelection from "./InfrastructureSelection.svelte";
  import RegistrySettings from "./RegistrySettings.svelte";
  import { submitForm } from "../lib/actions";
  import { terminal, pageUrl, timeLabel } from "../lib/format";
  import type { pageData } from "../lib/models";
  export let data: pageData;
  $: pending =
    Boolean(data.Settings.Operation.id) &&
    !terminal(data.Settings.Operation.status);
</script>

<div class="section-heading">
  <div>
    <h2>Shared settings</h2>
    <p>
      Registry credentials, offsite copies of S3 data, and configuration export.
    </p>
  </div>
  <a class="btn" data-variant="outline" href="/api/v1/management/configuration"
    >Export configuration</a
  >
</div>

{#if pending}
  <p class="field-help" role="status">
    A configuration change is in progress. Follow its connection test and save
    progress above. Current saved storage settings stay active until testing
    succeeds.
  </p>
{/if}

<RegistrySettings {data} action="put-management-registry" />

<section class="card management-panel" aria-labelledby="backup-coverage-title">
  <header>
    <h2 id="backup-coverage-title">Backup coverage</h2>
    <p>
      Check whether each database has a recent backup. A successful offsite copy
      alone does not confirm this.
    </p>
    <a
      class="btn"
      data-variant="outline"
      href={pageUrl("settings", "", "", "checkBackups", "true")}
      >Check backup coverage</a
    >
  </header>
  {#if data.Settings.BackupCoverageError}
    <p role="alert">{data.Settings.BackupCoverageError}</p>
  {/if}
  {#if data.Settings.BackupCoverage}
    <p role="status">
      Checked {timeLabel(data.Settings.BackupCoverage.checkedAt)}. {data
        .Settings.BackupCoverage.state === "attention"
        ? "Some backups need attention."
        : "Current backup evidence was found for every database."}
    </p>
    <div class="table-scroll">
      <table>
        <thead
          ><tr
            ><th>Type</th><th>Database</th><th>Status</th><th>Latest backup</th
            ><th>Details</th></tr
          ></thead
        >
        <tbody>
          {#each data.Settings.BackupCoverage.components as component}
            <tr>
              <td>{component.kind}</td><td>{component.name}</td><td
                >{component.state}</td
              >
              <td
                >{component.lastSuccess
                  ? timeLabel(component.lastSuccess)
                  : "None observed"}</td
              >
              <td>{component.message}</td>
            </tr>
          {/each}
        </tbody>
      </table>
    </div>
    <p class="field-help">
      Archive evidence does not replace a restore test. Refresh this check to
      inspect the archives again.
    </p>
  {/if}
</section>

<section class="card management-panel">
  <header>
    <h2>Offsite copy of S3 data</h2>
    <p>
      Copies completed management backups and PostgreSQL archives from primary
      S3 storage to a separate external S3 destination. Offsite copies are
      encrypted.
    </p>
    <p class="field-help">
      Keep the repository password and provider credentials outside Borq so you
      can recover if the management cluster is lost.
    </p>
  </header>
  <fieldset class="form-grid storage-fields" disabled={pending}>
    <form
      data-offsite-settings
      class="form-grid"
      action="/api/ui/actions"
      method="post"
      autocomplete="off"
      use:submitForm={{}}
    >
      <InfrastructureSelection data={data.Infrastructure} /><input
        type="hidden"
        name="action"
        value="put-management-offsite"
      /><label class="checkbox-label"
        ><input
          class="checkbox"
          type="checkbox"
          name="enabled"
          checked={data.Settings.Offsite.enabled}
        />Enable offsite backups</label
      ><label
        >Offsite S3 destination<input
          class="input"
          name="repository"
          type="url"
          required
          value={data.Settings.Offsite.repository.replace(/^s3:/, "")}
          placeholder="https://s3.us-east-1.amazonaws.com/my-backups/borq"
        /></label
      >
      <p class="field-help">
        Enter the HTTPS address of an existing S3 bucket, including an optional
        folder. In the example, my-backups is the bucket and borq is the folder.
        Use a bucket separate from primary storage.
      </p>
      <label
        >Offsite region<input
          class="input"
          name="region"
          required
          value={data.Settings.Offsite.region}
          placeholder="us-east-1"
        /></label
      ><label
        >Copy interval<input
          class="input"
          name="interval"
          required
          value={data.Settings.Offsite.interval}
          placeholder="6h"
        /></label
      >
      <p class="field-help">Use a duration such as 6h or 24h.</p>
      <label
        >Offsite snapshots to retain<input
          class="input"
          name="keepLast"
          type="number"
          min="1"
          max="10000"
          step="1"
          required
          value={data.Settings.Offsite.keepLast}
        /></label
      >
      {#if data.Settings.Offsite.credentialsRef}
        <p class="field-help">
          Credentials are saved securely in OpenBao and reused automatically.
        </p>
      {/if}
      <details
        class="storage-credentials"
        open={!data.Settings.Offsite.credentialsRef}
      >
        <summary
          >{data.Settings.Offsite.credentialsRef
            ? "Change offsite credentials"
            : "Offsite backup credentials"}</summary
        >
        <div class="form-grid">
          <label>
            <span id="offsite-password-label">Backup encryption password</span>
            <input
              class="input"
              name="password"
              type="password"
              required={!data.Settings.Offsite.credentialsRef}
              autocomplete="new-password"
              aria-labelledby="offsite-password-label"
              aria-describedby="offsite-password-help"
            />
            <small id="offsite-password-help">
              Keep a secure copy of this password outside Borq. If the password
              is lost, your offsite backups cannot be decrypted or recovered.
            </small></label
          ><label
            >Offsite access key<input
              class="input"
              name="accessKey"
              required={!data.Settings.Offsite.credentialsRef}
              autocomplete="off"
            /></label
          ><label
            >Offsite secret key<input
              class="input"
              name="secretKey"
              type="password"
              required={!data.Settings.Offsite.credentialsRef}
              autocomplete="new-password"
            /></label
          ><label
            >Offsite session token, optional<input
              class="input"
              name="sessionToken"
              type="password"
              autocomplete="new-password"
            /></label
          >
          <p class="field-help">
            Borq stores these credentials in OpenBao and manages their reference
            automatically. Leave all fields blank to keep saved credentials. To
            replace them, enter the encryption password and both provider keys.
            Keep your own copy for recovery.
          </p>
        </div>
      </details>
      <div class="form-actions">
        <button class="btn" type="submit">Test and save</button>
      </div>
      <p class="field-help">
        Borq tests this S3 connection in the background before saving changes. A
        failed test leaves your saved settings in place.
      </p>
    </form>
    <form action="/api/ui/actions" method="post" use:submitForm={{}}>
      <InfrastructureSelection data={data.Infrastructure} /><input
        type="hidden"
        name="action"
        value="disable-management-offsite"
      /><button
        class="btn"
        data-variant="outline"
        type="submit"
        disabled={!data.Settings.Offsite.enabled}
        >Disable offsite backups</button
      >
      <p class="field-help">
        Disabling stops future copies and keeps existing offsite snapshots and
        stored credentials. Primary S3 data stays in place.
      </p>
    </form>
  </fieldset>
</section>

<style>
  .storage-fields {
    border: 0;
    margin: 0;
    min-width: 0;
    padding: 0;
  }

  .storage-credentials[open] > summary {
    margin-bottom: 16px;
  }
</style>
