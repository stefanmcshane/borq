<script lang="ts">
  import { onMount, tick } from "svelte";
  import { generateWordName } from "../lib/name-generator";
  import {
    defaultPostgresConfiguration,
    editPostgresConfiguration,
    parsePostgresConfiguration,
    type PostgresSettingEdit,
  } from "../lib/postgres-editor";

  export let id: string;
  export let create = false;
  export let configuration = defaultPostgresConfiguration;
  export let suggestedNameMaxLength = 10;
  export let valid = true;

  let draft = configuration;
  let parsed = parsePostgresConfiguration(draft);
  let settings =
    parsed.kind === "valid"
      ? parsed.settings
      : {
          backups: true,
          restoreSchedule: "0 3 * * 0",
          instances: "1",
          storage: "1Gi",
        };
  let selectedTab: "settings" | "yaml" =
    parsed.kind === "valid" ? "settings" : "yaml";
  let name = "";
  let source: HTMLTextAreaElement;
  let submitted: HTMLInputElement;
  let advanced: HTMLDetailsElement;
  let editor: HTMLDivElement;

  $: valid = parsed.kind === "valid";
  $: canonical = parsed.kind === "valid" ? parsed.document.toString() : draft;
  $: restoreScheduleEnabled = valid && settings.backups;
  $: if (source)
    source.setCustomValidity(
      valid ? "" : "Correct the YAML configuration before continuing.",
    );

  onMount(() => {
    if (create && !name) name = generateWordName(suggestedNameMaxLength);
  });

  function updateName(input: HTMLInputElement) {
    const start = input.selectionStart;
    const end = input.selectionEnd;
    const direction = input.selectionDirection ?? undefined;
    name = input.value.replace(/[A-Z]/g, (letter) => letter.toLowerCase());
    input.value = name;
    if (start !== null && end !== null)
      input.setSelectionRange(start, end, direction);
  }

  function parseDraft(project = true) {
    parsed = parsePostgresConfiguration(draft);
    if (parsed.kind === "valid" && project) settings = parsed.settings;
    if (parsed.kind === "invalid") selectedTab = "yaml";
    source?.setCustomValidity(
      parsed.kind === "valid"
        ? ""
        : "Correct the YAML configuration before continuing.",
    );
  }

  function updateSetting(edit: PostgresSettingEdit, control: HTMLInputElement) {
    switch (edit.name) {
      case "backups":
        settings.backups = edit.value;
        break;
      case "restoreSchedule":
        settings.restoreSchedule = edit.value;
        break;
      case "instances":
        settings.instances = edit.value;
        break;
      case "storage":
        settings.storage = edit.value;
        break;
    }
    if (parsed.kind !== "valid" || !control.validity.valid) return;
    draft = editPostgresConfiguration(parsed, edit);
    parseDraft(false);
  }

  function selectTab(tab: "settings" | "yaml", focus = false) {
    selectedTab = valid ? tab : "yaml";
    if (focus)
      void tick().then(() =>
        editor
          .querySelector<HTMLButtonElement>(
            `[data-postgres-tab="${selectedTab}"]`,
          )
          ?.focus(),
      );
  }

  function tabKey(event: KeyboardEvent) {
    if (!["ArrowLeft", "ArrowRight", "Home", "End"].includes(event.key)) return;
    event.preventDefault();
    selectTab(
      event.key === "Home"
        ? "settings"
        : event.key === "End"
          ? "yaml"
          : selectedTab === "settings"
            ? "yaml"
            : "settings",
      true,
    );
  }

  function editorEvents(node: HTMLElement) {
    const form = node.closest("form");
    function reveal(event: Event) {
      const target = event.target;
      if (
        !(
          target instanceof HTMLInputElement ||
          target instanceof HTMLTextAreaElement
        ) ||
        !node.contains(target)
      )
        return;
      if (advanced) advanced.open = true;
      selectTab(target === source ? "yaml" : "settings");
      void tick().then(() => target.focus());
    }
    function submit(event: SubmitEvent) {
      parseDraft(false);
      if (parsed.kind === "valid") {
        submitted.value = parsed.document.toString();
        return;
      }
      event.preventDefault();
      event.stopImmediatePropagation();
      if (advanced) advanced.open = true;
      selectTab("yaml");
      void tick().then(() => source.reportValidity());
    }
    form?.addEventListener("invalid", reveal, true);
    form?.addEventListener("submit", submit, true);
    return {
      destroy() {
        form?.removeEventListener("invalid", reveal, true);
        form?.removeEventListener("submit", submit, true);
      },
    };
  }
</script>

<div class="postgres-editor form-grid" use:editorEvents>
  <input
    bind:this={submitted}
    type="hidden"
    name="configuration"
    value={canonical}
  />
  {#if create}
    <label
      >Name<input
        class="input"
        name="name"
        data-postgres-name
        data-postgres-name-suggestion={suggestedNameMaxLength}
        value={name}
        on:input={(event) => updateName(event.currentTarget)}
        autocapitalize="none"
        spellcheck="false"
        required
        maxlength="47"
        pattern={"[a-z0-9]([a-z0-9\\-]*[a-z0-9])?"}
        title="Use 1 to 47 lowercase letters, digits or hyphens. Start and end with a letter or digit."
        placeholder="database"
      /></label
    >
    <p class="field-help">
      Use 1 to 47 lowercase letters, digits or hyphens. Start and end with a
      letter or digit.
    </p>
  {/if}
  {#snippet contents()}
    <div class="postgres-editor-content" bind:this={editor}>
      <div
        class="application-subtabs postgres-settings-tabs"
        role="tablist"
        aria-label="Additional database settings"
      >
        <button
          id={`${id}-settings-tab`}
          type="button"
          role="tab"
          aria-selected={selectedTab === "settings"}
          aria-controls={`${id}-settings`}
          tabindex={selectedTab === "settings" ? 0 : -1}
          disabled={!valid}
          data-postgres-tab="settings"
          on:click={() => selectTab("settings")}
          on:keydown={tabKey}>Settings</button
        >
        <button
          id={`${id}-yaml-tab`}
          type="button"
          role="tab"
          aria-selected={selectedTab === "yaml"}
          aria-controls={`${id}-yaml`}
          tabindex={selectedTab === "yaml" ? 0 : -1}
          data-postgres-tab="yaml"
          on:click={() => selectTab("yaml")}
          on:keydown={tabKey}>YAML</button
        >
      </div>
      <div
        id={`${id}-settings`}
        class="form-grid"
        role="tabpanel"
        aria-labelledby={`${id}-settings-tab`}
        data-postgres-panel="settings"
        hidden={selectedTab !== "settings"}
      >
        <label class="checkbox-label"
          ><input
            class="checkbox"
            type="checkbox"
            data-postgres-setting="backups"
            checked={settings.backups}
            disabled={!valid}
            on:input={(event) =>
              updateSetting(
                { name: "backups", value: event.currentTarget.checked },
                event.currentTarget,
              )}
          />Backups enabled</label
        >
        <label
          >Restore test schedule in UTC<input
            class="input"
            data-postgres-setting="restoreSchedule"
            value={settings.restoreSchedule}
            required={restoreScheduleEnabled}
            disabled={!restoreScheduleEnabled}
            on:input={(event) =>
              updateSetting(
                { name: "restoreSchedule", value: event.currentTarget.value },
                event.currentTarget,
              )}
          /></label
        >
        <p class="field-help">
          Restore tests are mandatory while backups are enabled. The default
          schedule is every Sunday at 03:00 UTC.
        </p>
        <label
          >Instance count<input
            class="input"
            data-postgres-setting="instances"
            type="number"
            min="1"
            max="16"
            value={settings.instances}
            disabled={!valid}
            placeholder="1"
            on:input={(event) =>
              updateSetting(
                { name: "instances", value: event.currentTarget.value },
                event.currentTarget,
              )}
          /></label
        >
        <label
          >Storage per instance<input
            class="input"
            data-postgres-setting="storage"
            value={settings.storage}
            disabled={!valid}
            placeholder="1Gi"
            on:input={(event) =>
              updateSetting(
                { name: "storage", value: event.currentTarget.value },
                event.currentTarget,
              )}
          /></label
        >
      </div>
      <div
        id={`${id}-yaml`}
        role="tabpanel"
        aria-labelledby={`${id}-yaml-tab`}
        data-postgres-panel="yaml"
        hidden={selectedTab !== "yaml"}
      >
        <label
          >PostgreSQL configuration<textarea
            class="textarea code-editor"
            data-postgres-source
            bind:this={source}
            value={draft}
            rows={create ? 14 : 16}
            spellcheck="false"
            aria-invalid={!valid ? "true" : undefined}
            aria-describedby={`${id}-yaml-help ${id}-yaml-error`}
            on:input={(event) => {
              draft = event.currentTarget.value;
              parseDraft();
            }}></textarea></label
        >
        <p id={`${id}-yaml-help`} class="field-help">
          Settings and YAML edit the same configuration. Configure image,
          resources, storage class and backup options here.
        </p>
        <p
          id={`${id}-yaml-error`}
          class="field-help postgres-yaml-error"
          data-postgres-error
          role="alert"
          hidden={valid}
        >
          {parsed.kind === "invalid" ? parsed.problem : ""}
        </p>
      </div>
    </div>
  {/snippet}
  {#if create}
    <details class="postgres-advanced" bind:this={advanced}>
      <summary class="disclosure-button btn" data-variant="outline"
        >Advanced settings</summary
      >
      {@render contents()}
    </details>
  {:else}
    {@render contents()}
  {/if}
</div>
