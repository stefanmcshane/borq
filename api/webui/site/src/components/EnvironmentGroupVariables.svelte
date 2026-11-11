<script lang="ts">
  import UiScope from "./UiScope.svelte";
  import { submitForm } from "../lib/actions";
  import ComponentHost from "./ComponentHost.svelte";
  import { truth, pageUrl, variant } from "../lib/format";
  import type { pageData } from "../lib/models";
  export let data: pageData;
  let secretType = data.Secret.type;
  $: existing = Boolean(data.GroupLatestVersion);
  $: registry = secretType === "registry";
</script>

<section class="card management-panel environment-group-settings">
  <header class="panel-heading">
    <div>
      <h2>
        {#if truth(data.GroupLatestVersion)}Secret data{:else}Create a secret{/if}
      </h2>
      <p>
        {#if truth(data.Group.managedBy)}These values are read-only.
          Applications keep their pinned version until you review and deploy a
          new plan.{:else if truth(data.GroupLatestVersion)}Save changes to this
          secret. Applications adopt changes when you review and apply a
          deployment.{:else}Choose a secret type and add its key/value data.{/if}
      </p>
    </div>
    {#if truth(data.Group.version)}<span class="badge" data-variant="outline"
        >v{data.Group.version}</span
      >{/if}
  </header>
  <section>
    {#if truth(data.Group.managedBy)}
      {#if data.Group.version !== data.GroupLatestVersion}<p
          class="alert environment-version-notice"
        >
          Viewing version {data.Group.version}.
        </p>{/if}
      <div class="environment-rows">
        {#each data.Group.values as value}<div
            class="environment-row"
            role="group"
            aria-label="Environment variable"
          >
            <label
              >Key<input
                class="input mono"
                value={value.name}
                readonly
              /></label
            ><label
              >Value<textarea
                class="textarea environment-value mono"
                rows="1"
                wrap="off"
                readonly
                value={value.value}></textarea></label
            >
          </div>{/each}
      </div>
    {:else}
      {#if truth(data.Group.version) && truth(data.Group.version !== data.GroupLatestVersion)}<p
          class="alert environment-version-notice"
        >
          Viewing version {data.Group.version}. Publishing these values creates
          a new version; it does not change this saved version.
        </p>{/if}
      <form
        class="form-grid"
        action="/api/ui/actions"
        method="post"
        autocomplete="off"
        use:submitForm={{}}
      >
        <UiScope {data} />
        <input type="hidden" name="action" value="put-secret" />
        <input
          type="hidden"
          name="baseVersion"
          value={data.GroupLatestVersion}
        />
        <input type="hidden" name="groupVersion" value={data.Group.version} />
        <label
          >Secret type<select
            aria-label="Secret type"
            class="input"
            name="secretType"
            bind:value={secretType}
            disabled={existing}
          >
            <option value="secret-group">Secret group</option><option
              value="registry">Container registry</option
            >
          </select></label
        >
        {#if existing}<input
            type="hidden"
            name="secretType"
            value={secretType}
          />{/if}
        {#if existing}<input
            type="hidden"
            name="name"
            value={data.Group.name}
          />{:else}<label
            >Secret name<input
              class="input"
              name="name"
              aria-label="Secret name"
              value={data.Group.name}
              required
              maxlength="253"
              placeholder={registry ? "github-packages" : "web-config"}
              spellcheck="false"
            />{#if !registry}<small
                >Use a unique name in this space. Applications select this
                name.</small
              >{/if}</label
          >{/if}
        {#if registry}
          <input type="hidden" name="format" value="fields" />
          <div class="environment-rows">
            <div class="environment-row">
              <label
                >Key<input
                  class="input mono"
                  name="variableName"
                  value="server"
                  readonly
                /></label
              >
              <label
                >Value<input
                  class="input"
                  name="variableValue"
                  aria-label="Registry address"
                  value={data.Secret.data.server ?? ""}
                  placeholder="ghcr.io or registry.example.com:5000"
                  required
                  autocomplete="off"
                  spellcheck="false"
                /></label
              >
            </div>
            <div class="environment-row">
              <label
                >Key<input
                  class="input mono"
                  name="variableName"
                  value="username"
                  readonly
                /></label
              ><label
                >Value<input
                  class="input"
                  name="variableValue"
                  aria-label="Registry username"
                  value={data.Secret.data.username ?? ""}
                  required
                  autocomplete="off"
                /></label
              >
            </div>
            <div class="environment-row">
              <label
                >Key<input
                  class="input mono"
                  name="variableName"
                  value="password"
                  readonly
                /></label
              ><label
                >Value<input
                  class="input"
                  type="password"
                  name="variableValue"
                  aria-label="Registry password or token"
                  required={!existing}
                  autocomplete="new-password"
                /></label
              >
            </div>
          </div>
          <p class="field-help">
            The password or access token is write-only.{existing
              ? " Leave it blank to keep the saved credential. Enter it again if changing the registry address."
              : ""}
          </p>
        {:else}
          <ComponentHost
            initial={{
              component: "environment-editor",
              data: data.EnvironmentEditor,
            }}
          />
        {/if}
        <div class="form-actions">
          <button class="btn" type="submit" data-publish-group
            >{#if existing}Save secret{:else}Create secret{/if}</button
          ><a
            class="btn"
            data-variant="ghost"
            href={pageUrl("environment", data.Organization, data.Space.name)}
            >Cancel</a
          >
        </div>
      </form>
    {/if}
  </section>
</section>
