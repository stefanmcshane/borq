<script lang="ts">
  import { onMount } from "svelte";
  import { pageUrl } from "../lib/format";
  import { submitForm } from "../lib/actions";
  import type { pageData } from "../lib/models";
  import ManagedAddonNavigation from "./ManagedAddonNavigation.svelte";
  import SpacePocketIdEditor from "./SpacePocketIdEditor.svelte";
  import UiScope from "./UiScope.svelte";
  export let data: pageData;
  $: selected = data.SpacePocketId.Selected;
  $: pending =
    data.SpacePocketId.Services.some((service) =>
      ["pending", "deleting"].includes(service.state),
    ) || ["queued", "running"].includes(data.SpacePocketId.Operation.status);
  onMount(() => {
    const timer = window.setInterval(() => {
      if (pending) document.dispatchEvent(new CustomEvent("borq:refresh"));
    }, 3000);
    return () => window.clearInterval(timer);
  });
</script>

<ManagedAddonNavigation {data} />
<div class="section-heading">
  <div>
    <h2>Managed Pocket ID</h2>
    <p>
      Host a passkey identity provider in this space with persistent SQLite
      storage.
    </p>
    <p class="field-help">
      Rollovers stop the current instance before starting its replacement with
      the same database and encryption key. Sign-in pauses briefly during an
      update.
    </p>
  </div>
  {#if data.Space.id}<button
      class="btn"
      type="button"
      data-dialog-open="space-pocketid-create">Set up Pocket ID</button
    >{/if}
</div>
{#if !data.Space.id}
  <div class="resource-empty">
    <h3>Create a space first</h3>
    <p>Create or select a space before adding Pocket ID.</p>
    <a class="btn" href={pageUrl("spaces", data.Organization, "")}
      >Create a space</a
    >
  </div>
{:else}
  {#if data.SpacePocketId.Operation.id}<p role="status">
      {data.SpacePocketId.Operation.status} · {data.SpacePocketId.Operation
        .message}
    </p>{/if}
  {#if data.SpacePocketIdEnrollment}
    <section class="card management-panel">
      <header>
        <h2>Set up administrator passkey</h2>
        <p>
          This one-time link is shown only in this response. Open it privately
          to enroll the administrator.
        </p>
      </header>
      <a
        class="btn"
        href={data.SpacePocketIdEnrollment}
        target="_blank"
        rel="noopener noreferrer"
        referrerpolicy="no-referrer">Enroll administrator passkey</a
      >
    </section>
  {/if}
  <section class="card management-panel">
    <section>
      {#if data.SpacePocketId.Services.length}
        <div class="table-scroll">
          <table class="table">
            <thead><tr><th>Name</th><th>State</th><th>Address</th></tr></thead
            ><tbody
              >{#each data.SpacePocketId.Services as service}<tr
                  ><td
                    ><a
                      class="text-link"
                      href={pageUrl(
                        "space-pocketid",
                        data.Organization,
                        data.Space.name,
                        "spacePocketId",
                        service.name,
                      )}>{service.name}</a
                    ></td
                  ><td
                    >{service.state}{#if service.message}<p class="field-help">
                        {service.message}
                      </p>{/if}</td
                  ><td
                    ><a
                      class="text-link"
                      href={service.specification.issuerUrl}
                      target="_blank"
                      rel="noopener noreferrer">Open Pocket ID</a
                    ></td
                  ></tr
                >{/each}</tbody
            >
          </table>
        </div>
      {:else}<div class="resource-empty">
          <h3>No Pocket ID instances yet</h3>
          <p>Choose a hostname and administrator to get started.</p>
          <button
            class="btn"
            type="button"
            data-dialog-open="space-pocketid-create">Set up Pocket ID</button
          >
        </div>{/if}
    </section>
  </section>
  {#if selected.name}
    <section class="card management-panel">
      <header>
        <h2>{selected.name} settings</h2>
        <p>{selected.state} · {selected.message}</p>
      </header>
      {#if selected.state === "ready"}<form
          action="/api/ui/actions"
          method="post"
          use:submitForm={{}}
        >
          <UiScope {data} /><input
            type="hidden"
            name="name"
            value={selected.name}
          /><input
            type="hidden"
            name="action"
            value="enroll-space-pocketid"
          /><button class="btn" type="submit"
            >Set up administrator passkey</button
          >
          <p class="field-help">
            Available until the initial administrator has registered a passkey.
          </p>
        </form>{/if}
      {#key selected.id + selected.updatedAt}<SpacePocketIdEditor
          {data}
        />{/key}
      <details>
        <summary>Remove Pocket ID</summary>
        <p>
          The service will stop. Borq retains its SQLite volume and encryption
          key for recovery. To resume it later, set up the same name and choose
          “Resume retained data” in advanced settings.
        </p>
        <form action="/api/ui/actions" method="post" use:submitForm={{}}>
          <UiScope {data} /><input
            type="hidden"
            name="name"
            value={selected.name}
          /><input
            type="hidden"
            name="action"
            value="delete-space-pocketid"
          /><button class="btn" data-variant="destructive" type="submit"
            >Remove Pocket ID and retain data</button
          >
        </form>
      </details>
    </section>
  {/if}
  <dialog
    id="space-pocketid-create"
    class="workspace-dialog"
    aria-labelledby="space-pocketid-create-title"
  >
    <header class="dialog-heading">
      <div>
        <h2 id="space-pocketid-create-title">Set up Pocket ID</h2>
        <p>
          Borq provisions SQLite storage, encryption keys and HTTPS, then
          creates your administrator.
        </p>
      </div>
      <button class="btn" data-variant="ghost" type="button" data-dialog-close
        >Close</button
      >
    </header>
    {#key data.Organization + "/" + data.Space.name}<SpacePocketIdEditor
        {data}
        create
      />{/key}
  </dialog>
{/if}
