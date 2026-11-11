<script lang="ts">
  import UiIcon from "./UiIcon.svelte";
  import { truth } from "../lib/format";
  import type { applicationEditorData } from "../lib/models";
  export let data: applicationEditorData;
</script>

<fieldset class="application-section">
  <legend><UiIcon data={"applications"} /> Container</legend>
  <p class="field-help">Choose what runs and how much capacity it needs.</p>
  <div class="form-grid two-columns">
    <label
      >Application name<input
        class="input"
        name="name"
        value={data.Draft.name}
        readonly={truth(data.ExistingName)}
        required
        maxlength="50"
        pattern="[a-z]([a-z0-9-]*[a-z0-9])?"
        placeholder="web"
        spellcheck="false"
      /><small
        >A unique name in this space. It cannot be changed after deployment.</small
      ></label
    >
    <label
      >Container image<input
        class="input mono"
        name="image"
        required
        value={data.Draft.image}
        placeholder="ghcr.io/example/web:v1"
        spellcheck="false"
      /><small>Use an existing image from your container registry.</small
      ></label
    >
    <label
      >Container port<input
        class="input"
        type="number"
        name="port"
        min="0"
        max="65535"
        required
        value={data.Draft.port}
      /><small>Use 0 for a worker without a network service.</small></label
    >
    <label
      >Replicas<input
        class="input"
        type="number"
        name="replicas"
        min="0"
        max="2147483647"
        required
        value={data.Draft.replicas}
      /><small>Number of instances. Use 0 to scale down.</small></label
    >
    <label class="full-width"
      >Command<textarea
        class="textarea mono"
        name="command"
        rows="2"
        placeholder="Use the image's default command"
        spellcheck="false"
        value={data.Draft.command}></textarea><small
        >Executable and arguments, with quotes where needed. Leave empty to use
        the image defaults.</small
      ></label
    >
    <div class="application-storage full-width">
      <input type="hidden" name="sqliteFields" value="true" />
      {#if data.SqliteRetained}
        <p class="field-help">SQLite storage is enabled for this application.</p>
      {:else}
        <label class="checkbox-label">
          <input
            class="checkbox"
            type="checkbox"
            name="sqliteEnabled"
            aria-describedby="application-sqlite-help"
            checked={data.Draft.sqlite.enabled}
          />
          Enable SQLite
        </label>
      {/if}
      {#if data.SqliteRetained || data.Draft.sqlite.enabled}
        <input type="hidden" name="sqliteBackupFields" value="true" />
        <label class="checkbox-label">
          <input
            class="checkbox"
            type="checkbox"
            name="sqliteBackups"
            aria-describedby="application-sqlite-help"
            checked={data.Draft.sqlite.backups}
          />
          Continuous backups with Litestream
        </label>
      {/if}
      <p id="application-sqlite-help" class="field-help">
        Apps that support SQLite can read <code>SQLITE_DATABASE_PATH</code> for their
        database at <code>/var/lib/borq/sqlite/app.sqlite</code>. SQLite apps use one
        replica and cannot autoscale. Use managed PostgreSQL for apps that need
        multiple replicas. Backups are stored in management object storage.
      </p>
    </div>
  </div>
</fieldset>
