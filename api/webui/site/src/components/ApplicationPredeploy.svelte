<script lang="ts">
  import UiIcon from "./UiIcon.svelte";
  import { terminal, variant } from "../lib/format";
  import type { applicationEditorData } from "../lib/models";
  export let data: applicationEditorData;
</script>

<fieldset class="application-section">
  <legend
    ><UiIcon data={"terminal"} /> Predeploy task
    <span class="badge" data-variant="outline">Optional</span></legend
  >
  <p class="field-help">
    Run migrations or setup when deploying this app. The task uses this app’s
    image and environment and must exit successfully.
  </p>
  <div class="form-grid">
    <label
      >Predeploy command<textarea
        class="textarea mono"
        name="predeployCommand"
        rows="2"
        placeholder="./app migrate"
        spellcheck="false"
        value={data.Draft.predeploy.command}></textarea><small
        >Leave empty to disable the task.</small
      ></label
    >
    <div class="form-grid two-columns">
      <label
        >Task timeout (seconds)<input
          class="input"
          type="number"
          name="predeployTimeoutSeconds"
          min="1"
          max="2147483647"
          required
          value={data.Draft.predeploy.timeoutSeconds}
        /><small>Total time allowed, including scheduling and retries.</small
        ></label
      >
      <label
        >Retry limit<input
          class="input"
          type="number"
          name="predeployBackoffLimit"
          min="0"
          max="2147483647"
          required
          value={data.Draft.predeploy.backoffLimit}
        /><small
          >Number of failed attempts to retry. Use 0 for no retries.</small
        ></label
      >
    </div>
  </div>
</fieldset>
