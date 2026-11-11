<script lang="ts">
  import ApplicationConfigurationTabs from "./ApplicationConfigurationTabs.svelte";
  import UiIcon from "./UiIcon.svelte";
  import UiPlan from "./UiPlan.svelte";
  import UiScope from "./UiScope.svelte";
  import WorkloadLogs from "./WorkloadLogs.svelte";
  import WorkloadMetrics from "./WorkloadMetrics.svelte";
  import { loadPanel, submitForm } from "../lib/actions";
  import ComponentHost from "./ComponentHost.svelte";
  import { truth, pageUrl, yaml, variant } from "../lib/format";
  import type { pageData } from "../lib/models";
  export let data: pageData;
  $: operationScope =
    "?" +
    new URLSearchParams({
      organization: data.Organization,
      space: data.Space.name,
      app: data.Application.name,
    }).toString();
  $: recoveryActions = [
    {
      label: "View logs",
      href: pageUrl(
        "application",
        data.Organization,
        data.Space.name,
        "app",
        data.Application.name,
        "tab",
        "observability",
        "subtab",
        "logs",
      ),
    },
    {
      label: "Review configuration",
      href: pageUrl(
        "application",
        data.Organization,
        data.Space.name,
        "app",
        data.Application.name,
        "tab",
        "configuration",
      ),
    },
  ];
</script>

{#if !truth(data.Space.cluster)}
  <section class="alert">
    <h2>Create a space before deploying</h2>
    <p>Choose a registered cluster when creating a space on the Spaces page.</p>
    <a
      class="btn"
      data-variant="outline"
      href={pageUrl("spaces", data.Organization, data.Space.name)}
      >Create space</a
    >
  </section>
{:else}
  {#if truth(data.Application.name)}
    <div class="application-identity">
      <div class="app-avatar"><UiIcon data={"applications"} /></div>
      <div>
        <p class="app-image mono">{data.Application.image}</p>
        <p class="field-help">
          {#if truth(data.Application.port)}Web application · port {data
              .Application.port}{:else}Background worker{/if} · {data
            .Application.replicas}
          {#if data.Application.replicas === 1}replica{:else}replicas{/if}
        </p>
      </div>
      <div class="application-identity-actions">
        {#if !truth(data.Plan.id)}<form
            action="/api/ui/actions"
            method="post"
            use:submitForm={{
              errorRecovery: {
                label: "Check application status",
                description:
                  "Check the current application state before requesting another restart.",
                href: pageUrl(
                  "application",
                  data.Organization,
                  data.Space.name,
                  "app",
                  data.Application.name,
                ),
              },
            }}
          >
            <UiScope {data} /><input
              type="hidden"
              name="action"
              value="restart-app"
            /><input
              type="hidden"
              name="application"
              value={data.Application.name}
            /><button
              class="btn"
              data-variant="outline"
              type="submit"
              {...truth(data.Application.replicas === 0)
                ? {
                    disabled: true,
                    title: "Scale above zero before restarting",
                  }
                : {}}>Restart</button
            >
          </form>{/if}
      </div>
    </div>
  {/if}
  <nav class="application-tabs" aria-label="Application sections">
    {#if truth(data.Application.name)}<a
        href={pageUrl(
          "application",
          data.Organization,
          data.Space.name,
          "app",
          data.Application.name,
        )}
        aria-current={truth(data.ApplicationTab === "overview")
          ? "page"
          : undefined}>Overview</a
      >{/if}
    <a
      href={pageUrl(
        "application",
        data.Organization,
        data.Space.name,
        "app",
        data.Draft.name,
        "tab",
        "configuration",
      )}
      aria-current={truth(data.ApplicationTab === "configuration")
        ? "page"
        : undefined}
      >{#if truth(data.Application.name)}Configuration{:else}Install application{/if}</a
    >
    {#if truth(data.Application.name)}<a
        href={pageUrl(
          "application",
          data.Organization,
          data.Space.name,
          "app",
          data.Application.name,
          "tab",
          "observability",
        )}
        aria-current={truth(data.ApplicationTab === "observability")
          ? "page"
          : undefined}>Observability</a
      >{/if}
  </nav>
  {#if truth(data.Plan.id)}<UiPlan {data} />{:else}
    {#if truth(data.Operation.id)}<ComponentHost
        initial={{ component: "ui-operation", data: data.Operation }}
        {operationScope}
        {recoveryActions}
      />{/if}
    {#if data.ApplicationTab === "overview"}
      <div
        use:loadPanel={{
          url: data.AppPath + "&panel=runtime",
          lazy: false,
          interval: 0,
        }}
      >
        <section class="card management-panel">
          <section>
            <p class="field-help" role="status">
              Reading live application health…
            </p>
          </section>
        </section>
      </div>
      <div class="application-overview-grid">
        <section class="card management-panel">
          <header>
            <h2>Access</h2>
            <p>Your application’s addresses.</p>
          </header>
          <section>
            {#each data.Application.ingresses as ingresse}<a
                class="app-public-address"
                href={"https://" + ingresse.host + ingresse.path}
                target="_blank"
                rel="noopener noreferrer">{ingresse.host}{ingresse.path} ↗</a
              >{/each}{#if truth(data.Application.port)}<span class="field-help"
                >Internal address</span
              ><code class="internal-address"
                >http://{data.Application.name}.{data.Space.namespace}.svc:{data
                  .Application.port}</code
              >{:else}<strong class="field-help">No network service</strong>
              <p class="field-help">
                This worker runs without an exposed port.
              </p>{/if}
          </section>
        </section>
        <section class="card management-panel">
          <header>
            <h2>Scale</h2>
            <p>Choose how many instances run. Zero stops the application.</p>
          </header>
          <section>
            <form
              class="scale-form"
              action="/api/ui/actions"
              method="post"
              use:submitForm={{
                errorRecovery: {
                  label: "Review replica count",
                  description:
                    "Check the replica count, then review the change again.",
                  focus: "#scale-replicas",
                },
              }}
            >
              <UiScope {data} /><input
                type="hidden"
                name="action"
                value="scale-app"
              /><input
                type="hidden"
                name="application"
                value={data.Application.name}
              /><label for="scale-replicas" class="sr-only">Replicas</label
              ><input
                id="scale-replicas"
                class="input"
                type="number"
                name="replicas"
                min="0"
                max="2147483647"
                required
                value={data.Application.replicas}
              /><span class="field-help">replicas</span><button
                class="btn"
                type="submit"
                data-variant="outline">Review scale change</button
              >
            </form>
          </section>
        </section>
      </div>
      <div
        use:loadPanel={{
          url: data.AppPath + "&panel=deployments",
          lazy: false,
          interval: 0,
        }}
      >
        <p class="panel-loading" role="status">Loading deployment history…</p>
      </div>
    {:else if data.ApplicationTab === "observability"}
      <nav class="application-subtabs" aria-label="Observability views">
        <a
          href={pageUrl(
            "application",
            data.Organization,
            data.Space.name,
            "app",
            data.Application.name,
            "tab",
            "observability",
            "subtab",
            "metrics",
          )}
          aria-current={truth(data.ApplicationSubtab === "metrics")
            ? "page"
            : undefined}>Metrics</a
        ><a
          href={pageUrl(
            "application",
            data.Organization,
            data.Space.name,
            "app",
            data.Application.name,
            "tab",
            "observability",
            "subtab",
            "logs",
          )}
          aria-current={truth(data.ApplicationSubtab === "logs")
            ? "page"
            : undefined}>Logs</a
        >
      </nav>
      {#if data.ApplicationSubtab === "metrics"}
        <WorkloadMetrics data={data.Monitoring} />
      {:else}
        <WorkloadLogs data={data.Monitoring} />
      {/if}
    {:else}
      {#if !truth(data.Application.name)}<div class="install-intro">
          <span class="badge" data-variant="outline">1 · Configure</span><span
            >→</span
          ><span>2 · Review changes</span><span>→</span><span
            >3 · Deploy & monitor</span
          >
        </div>{/if}
      <section class="card management-panel application-settings">
        <header>
          <h2>
            {#if truth(data.Application.name)}Application configuration{:else}Bring
              your application online{/if}
          </h2>
          <p>
            {#if truth(data.Application.name)}Update this application’s
              settings, then review the changes before deploying.{:else}Start
              with a container image. Networking, health checks, logs, and
              metrics are built in.{/if}
          </p>
        </header>
        <section>
          {#if data.ApplicationSubtab === "manifests"}
            <ApplicationConfigurationTabs data={data.ApplicationEditor} />
            <h3>Kubernetes Manifests</h3>
            <p class="field-help">
              Kubernetes resources saved for {data.Application.name}.
            </p>
            {#if truth(data.ApplicationManifests)}{#each data.ApplicationManifests as manifest}<pre>{yaml(
                    manifest,
                  )}</pre>{/each}{:else}<p class="empty-copy">
                No manifests have been saved for this application.
              </p>{/if}
          {:else}
            <form
              class="form-grid"
              action="/api/ui/actions"
              method="post"
              use:submitForm={{
                errorRecovery: {
                  label: "Review configuration",
                  description:
                    "Your edits are still here. Update the settings, then review the changes again.",
                  focus: "#application-editor",
                },
              }}
            >
              <UiScope {data} /><input
                type="hidden"
                name="action"
                value="plan-app"
              />
              <ComponentHost
                initial={{
                  component: "application-editor",
                  data: data.ApplicationEditor,
                }}
              />
              <div class="form-actions">
                <button class="btn" type="submit" data-review-application
                  >Review changes</button
                ><a
                  class="btn"
                  data-variant="ghost"
                  href={pageUrl(
                    "applications",
                    data.Organization,
                    data.Space.name,
                  )}>Cancel</a
                ><span class="field-help"
                  >Your app changes when you deploy the reviewed plan.</span
                >
              </div>
            </form>
          {/if}
        </section>
      </section>
      {#if truth(data.Application.name) && data.ApplicationTab === "configuration"}
        <details class="card management-panel">
          <summary data-variant="destructive">Delete application</summary>
          <form
            action="/api/ui/actions"
            method="post"
            use:submitForm={{
              confirm:
                "Delete " +
                data.Application.name +
                " from " +
                data.Space.name +
                "? Its containers, services and routes will be removed. SQLite volumes, backups and secrets will be retained.",
            }}
          >
            <UiScope {data} />
            <input type="hidden" name="action" value="delete-app" />
            <input
              type="hidden"
              name="application"
              value={data.Application.name}
            />
            <p>
              Remove this application’s containers, services and routes. SQLite
              volumes, backups and secrets are retained. Deletion cannot be
              canceled once it starts.
            </p>
            <button class="btn" data-variant="destructive" type="submit"
              >Delete application</button
            >
          </form>
        </details>
      {/if}
    {/if}
  {/if}
{/if}
