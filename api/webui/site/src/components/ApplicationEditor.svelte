<script lang="ts">
  import ApplicationCheck from "./ApplicationCheck.svelte";
  import ApplicationConfigurationTabs from "./ApplicationConfigurationTabs.svelte";
  import ApplicationContainer from "./ApplicationContainer.svelte";
  import ApplicationEnvironment from "./ApplicationEnvironment.svelte";
  import ApplicationIngresses from "./ApplicationIngresses.svelte";
  import ApplicationPredeploy from "./ApplicationPredeploy.svelte";
  import UiIcon from "./UiIcon.svelte";
  import { truth, yaml } from "../lib/format";
  import type { applicationEditorData } from "../lib/models";
  export let data: applicationEditorData;
</script>

<div id="application-editor" class="form-grid">
  <input type="hidden" name="format" value={data.Format} />
  <input type="hidden" name="document" value={data.Document} />
  <input type="hidden" name="application" value={data.ExistingName} />
  <input type="hidden" name="sqliteRetained" value={data.SqliteRetained} />
  {#each data.SavedIngresses as savedIngress}<input
      type="hidden"
      name="savedIngress"
      value={savedIngress}
    />{/each}
  <input type="hidden" name="serviceNamespace" value={data.Namespace} />
  {#each data.GroupNames as groupName}<input
      type="hidden"
      name="environmentChoice"
      value={groupName}
    />{/each}
  <input type="hidden" name="subtab" value={data.Format} />
  <ApplicationConfigurationTabs {data} />
  {#if data.Format === "yaml"}
    <label
      >Application YAML<textarea
        class="textarea code-editor"
        name="configuration"
        rows="18"
        required
        spellcheck="false"
        aria-describedby="application-yaml-help"
        value={data.Yaml}></textarea></label
    >
    <p id="application-yaml-help" class="field-help">
      Enter one application, starting with <code>name:</code> and
      <code>image:</code>. Other applications in this space keep their settings.
    </p>
  {:else}
    <ApplicationContainer {data} />
  {/if}
  <ApplicationEnvironment {data} />
  {#if data.Format !== "yaml"}
    <ApplicationIngresses {data} />
    <fieldset class="application-section">
      <legend><UiIcon data={"health"} /> Health checks</legend>
      <p class="field-help">
        Probe endpoints and budgets are independent of the container port.
        Disabled checks retain their settings.
      </p>
      <div class="application-probes">
        {#each data.Checks as check}<ApplicationCheck data={check} />{/each}
      </div>
    </fieldset>
    <ApplicationPredeploy {data} />
  {/if}
</div>
