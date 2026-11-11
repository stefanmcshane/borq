<script lang="ts">
  import EnvironmentRow from "./EnvironmentRow.svelte";
  import { truth, yaml, variant } from "../lib/format";
  import type { environmentEditorData } from "../lib/models";
  export let data: environmentEditorData;
</script>

<div id="environment-editor" class="environment-editor">
  <input type="hidden" name="format" value={data.Format} />
  <div class="environment-editor-heading">
    <span>Secret values</span>
    <button
      class="btn"
      data-variant="ghost"
      type="submit"
      name="editor"
      value={truth(data.Format === "yaml") ? "fields" : "yaml"}
      formaction="/api/ui/environment-editor"
      formnovalidate
      >{#if data.Format === "yaml"}Use key/value fields{:else}Enter as YAML{/if}</button
    >
  </div>
  {#if data.Format === "yaml"}
    <label
      >Variables YAML<textarea
        class="textarea code-editor"
        name="values"
        rows="10"
        required
        spellcheck="false"
        aria-describedby="environment-yaml-help"
        placeholder="- name: LOG_LEVEL&#10;  value: info"
        value={data.Yaml}></textarea></label
    >
    <p id="environment-yaml-help" class="field-help">
      Enter a list of name/value pairs. Start with <code>- name:</code>; no
      top-level key is needed. Use <code>[]</code> for an empty group.
    </p>
  {:else}
    <div class="environment-rows" data-environment-rows>
      {#each data.Values as variable}<EnvironmentRow
          data={variable}
        />{:else}<EnvironmentRow data={{ name: "", value: "" }} />{/each}
    </div>
    <template data-environment-template
      ><EnvironmentRow data={{ name: "", value: "" }} /></template
    >
    <div>
      <button class="environment-text-action" type="button" data-environment-add
        >+ Add Variable</button
      >
    </div>
  {/if}
</div>
