<script lang="ts">
  import UiScope from "./UiScope.svelte";
  import { submitForm } from "../lib/actions";

  import type { pageData } from "../lib/models";
  export let data: pageData;
</script>

<section class="card management-panel environment-group-settings">
  <header>
    <h2>Group settings</h2>
    <p>This group is scoped to {data.Organization} / {data.Space.name}.</p>
  </header>
  <section class="form-grid">
    <label
      >Group name<input class="input" value={data.Group.name} readonly /><small
        >Clone this group to use a different name.</small
      ></label
    >
  </section>
</section>
{#if !data.Group.managedBy}
  <section class="card management-panel environment-group-settings">
    <header>
      <h2>Clone this group</h2>
      <p>
        Copy a published version into an independent group. Applications
        continue using their current group.
      </p>
    </header>
    <section>
      <form
        class="form-grid two-columns"
        action="/api/ui/actions"
        method="post"
        use:submitForm={{}}
      >
        <UiScope {data} /><input
          type="hidden"
          name="action"
          value="clone-group"
        /><input type="hidden" name="sourceName" value={data.Group.name} />
        <label
          >New group name<input
            class="input"
            name="name"
            required
            maxlength="253"
            placeholder={data.Group.name + "-copy"}
            spellcheck="false"
          /></label
        >
        <label
          >Source version<input
            class="input"
            name="sourceVersion"
            required
            value="latest"
            pattern="latest|[1-9][0-9]*"
            spellcheck="false"
          /><small>Use latest or a published version number.</small></label
        >
        <div class="form-actions full-width">
          <button class="btn" type="submit">Clone group</button>
        </div>
      </form>
    </section>
  </section>
{/if}
