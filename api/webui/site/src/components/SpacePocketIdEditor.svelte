<script lang="ts">
  import { submitForm } from "../lib/actions";
  import type { pageData } from "../lib/models";
  import UiScope from "./UiScope.svelte";
  export let data: pageData;
  export let create = false;
  $: selected = data.SpacePocketId.Selected;
  $: settings = create
    ? {
        issuerUrl: "",
        adminUsername: "admin",
        adminEmail: "",
        storage: "",
        storageClass: "",
        image: "",
        certificateIssuer: "",
      }
    : selected.specification;
  $: editing = !create && !!selected.name;
  $: pending = !create && ["pending", "deleting"].includes(selected.state);
</script>

<form
  class="form-grid"
  action="/api/ui/actions"
  method="post"
  use:submitForm={{}}
>
  <UiScope {data} />
  <input type="hidden" name="action" value="put-space-pocketid" />
  {#if editing}<input type="hidden" name="apply" value="true" />{/if}
  <fieldset class="form-grid" disabled={pending}>
    <label
      >Name<input
        class="input"
        name="name"
        required
        maxlength="63"
        pattern={"[a-z0-9]([a-z0-9\\-]*[a-z0-9])?"}
        placeholder="identity"
        value={create ? "" : selected.name}
        readonly={editing}
      /></label
    >
    <label
      >Pocket ID address<input
        class="input"
        type="url"
        name="issuerUrl"
        required
        maxlength="2048"
        placeholder="https://id.example.com"
        value={settings.issuerUrl}
        readonly={editing}
      /></label
    >
    <p class="field-help">
      Use a dedicated HTTPS hostname pointing to this space's cluster. Passkeys
      require HTTPS.
    </p>
    <label
      >Administrator username<input
        class="input"
        name="adminUsername"
        required
        maxlength="50"
        pattern={"[a-zA-Z0-9]([a-zA-Z0-9_.@\\-]*[a-zA-Z0-9])?"}
        value={settings.adminUsername || "admin"}
        readonly={editing}
        autocomplete="username"
      /></label
    >
    <label
      >Administrator email<input
        class="input"
        type="email"
        name="adminEmail"
        required
        maxlength="254"
        value={settings.adminEmail}
        readonly={editing}
        autocomplete="email"
      /></label
    >
    <details>
      <summary>Advanced settings</summary>
      <div class="form-grid">
        {#if create}
          <label
            ><input type="checkbox" name="apply" value="true" /> Resume retained data
            for this name</label
          >
          <p class="field-help">
            Use the original name, address, administrator and storage settings
            to restart a previously removed instance.
          </p>
        {/if}
        <label
          >Storage size<input
            class="input"
            name="storage"
            value={settings.storage}
            placeholder="1Gi"
            readonly={editing}
          /></label
        >
        <label
          >Storage class<input
            class="input"
            name="storageClass"
            value={settings.storageClass}
            placeholder="Cluster default"
            readonly={editing}
          /></label
        >
        <label
          >Pocket ID image (with SHA-256 digest)<input
            class="input mono"
            name="image"
            value={settings.image}
            placeholder="Borq default"
          /></label
        >
        <label
          >Certificate issuer<input
            class="input"
            name="certificateIssuer"
            value={settings.certificateIssuer}
            placeholder="Cluster default"
          /></label
        >
      </div>
    </details>
    <div class="form-actions">
      <button class="btn" type="submit"
        >{pending
          ? "Updating Pocket ID…"
          : editing
            ? "Save and roll over"
            : "Set up Pocket ID"}</button
      >
    </div>
  </fieldset>
</form>

<style>
  fieldset {
    border: 0;
    padding: 0;
    min-width: 0;
  }
</style>
