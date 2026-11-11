<script lang="ts">
  import { submitForm } from "../lib/actions";
  import { truth, yaml } from "../lib/format";
  import type { pageData } from "../lib/models";
  import InfrastructureSelection from "./InfrastructureSelection.svelte";
  export let data: pageData;
</script>

<section class="card management-panel component-catalog" id="component-catalog">
  <header>
    <h2>Component configuration</h2>
    <p>
      Inspect the desired configuration Borq uses when installing or updating
      infrastructure.
    </p>
  </header>
  {#if truth(data.Settings.Components)}
    <div class="component-catalog-context">
      <span>Default namespace <code>{data.Settings.Catalog.namespace}</code></span><span
        >Profile <code>{data.Settings.Catalog.profile}</code></span
      >
    </div>
    <label class="component-mobile-picker"
      >Component<select
        class="select"
        data-settings-navigation
        aria-label="Component"
        >{#each data.Settings.Components as component}<option
            value={component.URL}
            selected={truth(component.Selected)}
            >{component.Name} · {component.Version}</option
          >{/each}</select
      ></label
    >
    <div class="component-catalog-layout">
      <nav class="component-list" aria-label="Components on selected cluster">
        {#each data.Settings.Components as component}<a
            href={component.URL}
            aria-current={component.Selected ? "true" : undefined}
            data-selected={component.Selected ? "true" : undefined}
            >{component.Name}<span>{component.Version}</span></a
          >{/each}
      </nav>
      {#if truth(data.Settings.Selected)}{@const value8 =
          data.Settings.Selected}
        <section
          class="component-detail"
          id="component-detail"
          aria-label={value8.name + " configuration"}
        >
          <header>
            <h3>{value8.name}</h3>
            <p>{value8.description}</p>
            {#if data.Infrastructure.ComponentScope === "postgres"}<p
                class="field-help"
              >
                Installed when needed by a managed PostgreSQL database.
              </p>{/if}
          </header>
          <dl class="component-metadata">
            <div>
              <dt>Repository</dt>
              <dd>
                {#if truth(value8.repository)}{value8.repository}{:else}Direct
                  chart archive{/if}
              </dd>
            </div>
            <div>
              <dt>Chart</dt>
              <dd>{value8.chart}</dd>
            </div>
            <div>
              <dt>Exact version</dt>
              <dd>{value8.version}</dd>
            </div>
            <div>
              <dt>Namespace</dt>
              <dd>{value8.namespace || data.Settings.Catalog.namespace}</dd>
            </div>
            <div>
              <dt>Profile</dt>
              <dd>{data.Settings.Catalog.profile}</dd>
            </div>
          </dl>
          <section class="component-effective-values">
            <h4>Values Borq sends to Helm</h4>
            <p class="field-help">
              Desired Borq defaults with saved overrides applied. Upstream chart
              defaults are not included. Installed releases may differ.
            </p>
            <pre
              id="component-effective-values"
              role="region"
              aria-label="Values Borq sends to Helm">{yaml(value8.values)}</pre>
            <p class="field-help">
              Helm sets the release namespace separately from these values.
            </p>
          </section>
          <details class="component-customize">
            <summary class="btn disclosure-button" data-variant="outline"
              >{data.Infrastructure.Node === "application"
                ? "Customize shared defaults"
                : "Customize " + value8.name}</summary
            >
            <form
              class="form-grid"
              action="/api/ui/actions"
              method="post"
              use:submitForm={{}}
            >
              <InfrastructureSelection data={data.Infrastructure} />
              <input
                type="hidden"
                name="action"
                value="put-management-component"
              />
              <p class="field-help">
                Apply installs or updates this component across all applicable
                clusters. Changes are shared by every cluster using this
                component.
              </p>
              <div class="form-grid two-columns">
                <label class="full-width"
                  >Repository<input
                    class="input"
                    name="repo"
                    value={((value9) => (truth(value9) ? value9 : ""))(
                      value8.override.repo,
                    )}
                    placeholder={truth(value8.repository)
                      ? value8.repository
                      : "Direct chart archive"}
                  /></label
                ><label
                  >Chart<input
                    class="input"
                    name="chart"
                    value={((value10) => (truth(value10) ? value10 : ""))(
                      value8.override.chart,
                    )}
                    placeholder={value8.chart}
                  /></label
                ><label
                  >Exact version<input
                    class="input"
                    name="version"
                    value={((value11) => (truth(value11) ? value11 : ""))(
                      value8.override.version,
                    )}
                    placeholder={value8.version}
                  /></label
                >
              </div>
              <label
                >Your values overrides<textarea
                  class="textarea code-editor"
                  name="values"
                  rows="6"
                  spellcheck="false"
                  placeholder="# Add values to override Borq defaults"
                  value={truth(value8.override.values)
                    ? yaml(value8.override.values)
                    : ""}></textarea></label
              >
              <p class="field-help">
                Blank chart fields keep current settings. Blank values retain
                saved values. Use <code>&#123;&#125;</code> to clear values overrides.
              </p>
              <div class="form-actions">
                <button class="btn" type="submit">Apply {value8.name}</button>
              </div>
            </form>
          </details>
        </section>
      {/if}
    </div>{/if}
</section>
