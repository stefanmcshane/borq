<script lang="ts">
  import UiScope from "./UiScope.svelte";
  import { submitForm } from "../lib/actions";
  import type { pageData } from "../lib/models";
  export let data: pageData;
</script>

{#if !data.Group.managedBy}
  <section class="card management-panel">
    <header>
      <h2>Delete secret</h2>
      <p>
        {data.Secret.type === "registry"
          ? "Revoke all versions of this secret and remove their pull secrets. Running applications are not restarted; future image pulls will need new credentials."
          : "Delete this secret and all its versions. Secrets used by applications cannot be deleted."}
      </p>
    </header>
    <form action="/api/ui/actions" method="post" use:submitForm={{}}>
      <UiScope {data} /><input
        type="hidden"
        name="action"
        value="delete-secret"
      />
      <input type="hidden" name="name" value={data.Group.name} />
      <input type="hidden" name="baseVersion" value={data.GroupLatestVersion} />
      <button class="btn" data-variant="destructive" type="submit"
        >Delete secret</button
      >
    </form>
  </section>
{/if}
