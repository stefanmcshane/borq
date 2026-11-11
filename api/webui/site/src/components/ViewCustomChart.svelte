<script lang="ts">
  import UiIcon from "./UiIcon.svelte";
  import WorkloadLogs from "./WorkloadLogs.svelte";
  import WorkloadMetrics from "./WorkloadMetrics.svelte";
  import { loadPanel, requestButton, submitForm } from "../lib/actions";
  import { truth, pageUrl, variant } from "../lib/format";
  import type { pageData } from "../lib/models";
  export let data: pageData;
</script>

{#if truth(data.Space.id)}
  {#if truth(data.CustomChart.name)}
    <nav class="application-tabs" aria-label="Custom addon sections">
      <a
        href={pageUrl(
          "custom-chart",
          data.Organization,
          data.Space.name,
          "customChart",
          data.CustomChart.name,
        )}
        aria-current={truth(data.CustomChartTab === "overview")
          ? "page"
          : undefined}>Overview</a
      >
      <a
        href={pageUrl(
          "custom-chart",
          data.Organization,
          data.Space.name,
          "customChart",
          data.CustomChart.name,
          "tab",
          "configuration",
        )}
        aria-current={truth(data.CustomChartTab === "configuration")
          ? "page"
          : undefined}>Configuration</a
      >
      <a
        href={pageUrl(
          "custom-chart",
          data.Organization,
          data.Space.name,
          "customChart",
          data.CustomChart.name,
          "tab",
          "observability",
        )}
        aria-current={truth(data.CustomChartTab === "observability")
          ? "page"
          : undefined}>Observability</a
      >
    </nav>
  {/if}
  {#if data.CustomChartTab === "overview"}
    {#if truth(data.CustomChart.operation) && truth(truth(data.CustomChart.state === "pending") || truth(data.CustomChart.state === "deleting"))}<div
        use:loadPanel={{
          url: "/api/ui/custom-charts/operations/" + data.CustomChart.operation,
          lazy: false,
          interval: 0,
        }}
      >
        <p class="field-help">Loading installation progress…</p>
      </div>{/if}
    <section class="card management-panel">
      <header class="panel-heading">
        <div>
          <h2>
            {data.CustomChart.specification.chart}
            <span class="mono">{data.CustomChart.specification.version}</span>
          </h2>
          <p>Installed from {data.CustomChart.specification.repository}</p>
        </div>
        <span class="badge" data-variant={variant(data.CustomChart.state)}
          >{data.CustomChart.state}</span
        >
      </header>
      <section>
        <dl class="release-metadata">
          <div>
            <dt>Helm release</dt>
            <dd class="mono">{data.CustomChart.releaseName}</dd>
          </div>
          <div>
            <dt>Namespace</dt>
            <dd class="mono">{data.CustomChart.namespace}</dd>
          </div>
          <div>
            <dt>Revision</dt>
            <dd>{data.CustomChart.revision}</dd>
          </div>
        </dl>
        <p class="field-help">{data.CustomChart.message}</p>
      </section>
    </section>
    <div
      use:loadPanel={{
        url: data.Monitoring.Path + "&panel=events&range=1h",
        lazy: false,
        interval: 0,
      }}
    >
      <p class="panel-loading" role="status">
        Loading installation and pod events…
      </p>
    </div>
  {:else if data.CustomChartTab === "observability"}
    <nav class="application-subtabs" aria-label="Observability views">
      <a
        href={pageUrl(
          "custom-chart",
          data.Organization,
          data.Space.name,
          "customChart",
          data.CustomChart.name,
          "tab",
          "observability",
          "subtab",
          "metrics",
        )}
        aria-current={truth(data.CustomChartSubtab === "metrics")
          ? "page"
          : undefined}>Metrics</a
      ><a
        href={pageUrl(
          "custom-chart",
          data.Organization,
          data.Space.name,
          "customChart",
          data.CustomChart.name,
          "tab",
          "observability",
          "subtab",
          "logs",
        )}
        aria-current={truth(data.CustomChartSubtab === "logs")
          ? "page"
          : undefined}>Logs</a
      >
    </nav>
    {#if data.CustomChartSubtab === "metrics"}<WorkloadMetrics
        data={data.Monitoring}
      />{:else}<WorkloadLogs data={data.Monitoring} />{/if}
  {:else}
    <section class="card management-panel">
      <header>
        <h2>
          {#if truth(data.CustomChartDraft.Existing)}Addon settings{:else}Install
            a custom addon{/if}
        </h2>
        <p>
          Values override the selected chart’s defaults. Each custom addon
          belongs to this space.
        </p>
      </header>
      <section>
        <form
          class="form-grid"
          action="/api/ui/actions"
          method="post"
          data-custom-chart-editor
          use:submitForm={{}}
        >
          <input type="hidden" name="action" value="put-custom-chart" /><input
            type="hidden"
            name="view"
            value="custom-chart"
          /><input
            type="hidden"
            name="organization"
            value={data.Organization}
          /><input type="hidden" name="space" value={data.Space.name} />
          {#if truth(data.CustomChartDraft.Existing)}<input
              type="hidden"
              name="apply"
              value="true"
            />{/if}
          <label
            >Repository<span class="chart-repository-field"
              ><input
                class="input"
                name="repository"
                value={data.CustomChartDraft.Repository}
                required
                placeholder="https://charts.example.com or oci://registry.example.com/charts"
                autocomplete="off"
                aria-describedby="custom-chart-status"
              /><span
                class="chart-repository-indicator"
                data-state="idle"
                data-repository-indicator
                hidden
                aria-hidden="true"
                ><svg
                  class="ui-icon"
                  width="16"
                  height="16"
                  viewBox="0 0 24 24"
                  fill="none"
                  stroke="currentColor"
                  stroke-width="1.6"
                  stroke-linecap="round"
                  stroke-linejoin="round"><path></path></svg
                ></span
              ></span
            ><span
              class="field-help"
              id="custom-chart-status"
              role="status"
              aria-live="polite"
              >Enter a repository to choose a chart and version.</span
            ></label
          >
          <label
            >Chart<input
              class="input"
              name="chart"
              value={data.CustomChartDraft.Chart}
              required
              placeholder="redis"
              autocomplete="off"
            /></label
          >
          <label
            >Version<span class="chart-repository-field"
              ><input
                class="input"
                name="version"
                value={data.CustomChartDraft.Version}
                required
                placeholder="1.2.3"
                autocomplete="off"
                aria-describedby="custom-chart-version-status"
              /><span
                class="chart-repository-indicator"
                data-state="idle"
                data-version-indicator
                hidden
                aria-hidden="true"
                ><svg
                  class="ui-icon"
                  width="16"
                  height="16"
                  viewBox="0 0 24 24"
                  fill="none"
                  stroke="currentColor"
                  stroke-width="1.6"
                  stroke-linecap="round"
                  stroke-linejoin="round"><path></path></svg
                ></span
              ></span
            ><span
              class="field-help"
              id="custom-chart-version-status"
              role="status"
              aria-live="polite">Choose a chart to load versions.</span
            ></label
          >
          <label
            >Addon name<input
              class="input"
              name="name"
              value={data.CustomChartDraft.Name}
              required
              maxlength="47"
              pattern="[a-z0-9]([a-z0-9-]*[a-z0-9])?"
              readonly={truth(data.CustomChartDraft.Existing)}
              placeholder="my-service"
            /></label
          >
          <div class="form-full">
            <button
              class="btn"
              data-variant="outline"
              type="button"
              popovertarget="custom-chart-defaults-popout"
              popovertargetaction="show"
              data-custom-chart-defaults
              use:requestButton={{
                url: "/api/ui/custom-charts/defaults",
                method: "POST",
                target: "#custom-chart-defaults",
              }}>Show defaults</button
            >
            <div
              id="custom-chart-defaults-popout"
              class="chart-defaults-popout"
              popover="auto"
              role="dialog"
              aria-labelledby="chart-defaults-title"
            >
              <header>
                <div>
                  <h2 id="chart-defaults-title">Chart defaults</h2>
                  <p class="field-help">
                    Copy any settings you need into Values overrides.
                  </p>
                </div>
                <button
                  class="btn"
                  type="button"
                  data-variant="ghost"
                  popovertarget="custom-chart-defaults-popout"
                  popovertargetaction="hide"
                  aria-label="Close defaults"
                  data-focus><UiIcon data={"close"} /></button
                >
              </header>
              <div id="custom-chart-defaults" aria-live="polite"></div>
            </div>
          </div>
          <label class="form-full"
            >Values overrides<textarea
              class="textarea mono"
              name="values"
              rows="14"
              spellcheck="false"
              value={data.CustomChartDraft.Values}></textarea><span
              class="field-help"
              >Enter only your overrides. Saving replaces the complete override
              map.</span
            ></label
          >
          <label class="form-full"
            ><span
              ><input
                class="checkbox"
                type="checkbox"
                name="logs"
                checked={truth(data.CustomChartDraft.Logs)}
              /> Collect pod logs</span
            ><span class="field-help"
              >Disable for noisy charts to reduce log storage.</span
            ></label
          >
          <details class="form-full">
            <summary>Pod selection</summary><label
              >Pod template labels<textarea
                class="textarea mono"
                name="selector"
                rows="4"
                spellcheck="false"
                value={data.CustomChartDraft.Selector}></textarea></label
            >
            <p class="field-help">
              Use a YAML map of labels to narrow logs and resource usage
              collection. An empty map selects every pod in this custom addon.
            </p>
          </details>
          <div class="form-actions">
            <button class="btn" type="submit" data-custom-chart-save
              >{#if truth(data.CustomChartDraft.Existing)}Save and upgrade{:else}Install
                custom addon{/if}</button
            >
          </div>
        </form>
      </section>
    </section>
    {#if truth(data.CustomChart.name)}
      <details class="card management-panel">
        <summary data-variant="destructive">Remove custom addon</summary>
        <form
          action="/api/ui/actions"
          method="post"
          use:submitForm={{
            confirm:
              "Uninstall " +
              data.CustomChart.name +
              " from this space? Chart CRDs will be retained.",
          }}
        >
          <input
            type="hidden"
            name="action"
            value="delete-custom-chart"
          /><input
            type="hidden"
            name="organization"
            value={data.Organization}
          /><input type="hidden" name="space" value={data.Space.name} /><input
            type="hidden"
            name="name"
            value={data.CustomChart.name}
          />
          <p>This uninstalls the chart release. Chart CRDs are retained.</p>
          <button class="btn" data-variant="destructive" type="submit"
            >Uninstall custom addon</button
          >
        </form>
      </details>
    {/if}
  {/if}
{:else}<p class="empty-copy">Create a space to install custom addons.</p>{/if}
