<script lang="ts">
  import { flushSync } from "svelte";
  import UiIcon from "./UiIcon.svelte";
  import { truth } from "../lib/format";
  import type { applicationEditorData } from "../lib/models";
  export let data: applicationEditorData;
  $: draft = data.SecretDraft;
  $: values = data.SecretValues.map((variable) => ({ ...variable }));

  function changed() {
    // FormData must read the identity for the current values, even within one event.
    flushSync(() => {
      draft = Array.from(crypto.getRandomValues(new Uint8Array(16)), (byte) =>
        byte.toString(16).padStart(2, "0"),
      ).join("");
    });
  }

  function updateVariable(
    index: number,
    field: "name" | "value",
    value: string,
  ) {
    values = values.map((variable, position) =>
      position === index ? { ...variable, [field]: value } : variable,
    );
    changed();
  }

  function addVariable() {
    values = [...values, { name: "", value: "" }];
    changed();
  }

  function removeVariable(index: number) {
    values = values.filter((_, position) => position !== index);
    changed();
  }
</script>

<fieldset class="application-section">
  <legend><UiIcon data={"management"} /> Environment</legend>
  <input type="hidden" name="secretDraft" value={draft} />

  {#if data.Format === "yaml"}
    <p class="field-help">
      Set the secret group name and version in the YAML's <code>env</code> field.
    </p>
  {:else}
    <p class="field-help">
      The selected version is pinned when you review changes.
    </p>
    <div class="form-grid two-columns">
      <label
        >Secret group<select class="select" name="environment">
          <option
            value=""
            selected={truth(
              truth(data.Draft.env.name === "") ||
                truth(data.Draft.env.version === "0"),
            )}
            >{#if truth(data.ExistingName)}Keep current group{:else if values.length > 0}Custom
              secrets only{:else}App default{/if}</option
          >
          {#if truth(data.Draft.env.name) && truth(data.Draft.env.version !== "0")}<option
              value={data.Draft.env.name}
              selected>{data.Draft.env.name}</option
            >{/if}
          {#each data.GroupNames as groupName}{#if truth(groupName !== data.Draft.env.name) || truth(data.Draft.env.version === "0")}<option
                value={groupName}>{groupName}</option
              >{/if}{/each}
        </select><small
          >Select a published group in this space. <a
            class="text-link"
            href={data.EnvironmentUrl}>Manage groups</a
          ></small
        ></label
      >
      <label
        >Environment version<input
          class="input"
          name="environmentVersion"
          value={truth(data.Draft.env.version !== "0")
            ? data.Draft.env.version
            : ""}
          placeholder="latest"
          pattern="latest|[0-9]+"
          spellcheck="false"
        /><small
          >Choose latest or a published version number. You can change the
          version on its own.</small
        ></label
      >
    </div>
  {/if}
  <details open={values.length > 0}>
    <summary>Add custom secrets (optional)</summary>

    <p class="field-help">
      Add variables alongside the selected group. Reviewing changes saves an
      app-specific copy with these values overriding any matching keys. Values
      stay out of application YAML. For a new app, select a group to include its
      variables; leaving the group unset saves only your custom secrets.
    </p>
    <div class="environment-rows">
      {#each values as variable, index}
        <div class="environment-row" role="group" aria-label="App secret">
          <label
            >Key<input
              class="input mono"
              name="variableName"
              value={variable.name}
              on:input={(event) =>
                updateVariable(index, "name", event.currentTarget.value)}
              required
              placeholder="API_KEY"
              spellcheck="false"
              autocomplete="off"
            /></label
          >
          <label
            >Value<textarea
              class="textarea environment-value mono"
              name="variableValue"
              value={variable.value}
              on:input={(event) =>
                updateVariable(index, "value", event.currentTarget.value)}
              rows="1"
              wrap="off"
              spellcheck="false"
              autocomplete="off"></textarea></label
          >
          <button
            class="environment-text-action environment-remove"
            type="button"
            on:click={() => removeVariable(index)}>Remove secret</button
          >
        </div>
      {/each}
    </div>
    <button class="environment-text-action" type="button" on:click={addVariable}
      >+ Add secret</button
    >
  </details>
</fieldset>
