<script lang="ts">
  import { submitForm, requestButton } from "../lib/actions";
  import {
    truth,
    pageUrl,
    yaml,
    terminal,
    timeLabel,
    variant,
  } from "../lib/format";
  import type { applicationPanelData } from "../lib/models";
  export let data: applicationPanelData;
</script>

<section class="card management-panel">
  <header>
    <h2>Deployment history</h2>
    <p>
      Recent releases for {data.Application}. New deployments replace unfinished
      ones in this space. Use an older plan as the starting point for a new
      version.
    </p>
  </header>
  <section>
    {#if truth(data.Problem)}<p class="empty-copy">
        {data.Problem}
      </p>
      <button
        class="btn"
        data-variant="outline"
        type="button"
        use:requestButton={{
          url: data.Location,
          method: "GET",
          target: "closest .management-panel",
        }}>Try again</button
      >{:else if truth(data.Deployments)}<div class="release-list">
        {#each data.Deployments as deployment, deploymentIndex}
          <article class="release-item">
            <details
              class="release-details"
              id={"deployment-details-" + deployment.operation.id}
              data-deployment-details
            >
              <summary class="release-summary"
                ><span
                  class="status-dot"
                  data-state={deployment.operation.status}
                ></span><span class="release-main"
                  ><strong class="mono">{deployment.configuration.image}</strong
                  >
                  <span class="badge mono" data-variant="outline"
                    >{deployment.revision}</span
                  ><span class="field-help release-caption"
                    >{timeLabel(deployment.operation.startedAt)} · {deployment
                      .configuration.replicas} replicas ·
                    <span
                      class="badge"
                      data-variant={variant(deployment.operation.status)}
                      >{deployment.operation.status}</span
                    ></span
                  ><span class="field-help release-caption"
                    >{deployment.operation.message}</span
                  ></span
                ><span class="btn release-details-label" data-variant="outline"
                  >Details</span
                ></summary
              >
              <div class="release-details-content">
                <dl class="release-metadata">
                  <div>
                    <dt>Deployment</dt>
                    <dd class="mono">{deployment.operation.id}</dd>
                  </div>
                  <div>
                    <dt>Phase</dt>
                    <dd>{deployment.operation.phase}</dd>
                  </div>
                  <div>
                    <dt>Last updated</dt>
                    <dd>{timeLabel(deployment.operation.updatedAt)}</dd>
                  </div>
                </dl>
                {#if truth(deployment.operation.resources)}<div
                    class="table-scroll release-resource-list"
                  >
                    <table class="table">
                      <thead
                        ><tr
                          ><th>Resource</th><th>Status</th><th>Details</th></tr
                        ></thead
                      ><tbody
                        >{#each deployment.operation.resources as resource, resourceIndex}<tr
                            ><td>{resource.kind} / {resource.name}</td><td
                              >{resource.state}</td
                            ><td>{resource.message}</td></tr
                          >{/each}</tbody
                      >
                    </table>
                  </div>{/if}
                <details class="release-settings">
                  <summary>Release settings</summary>
                  <pre>{yaml(deployment.configuration)}</pre>
                </details>
              </div>
            </details>
            <div class="release-actions">
              {#if deployment.operation.status === "failed"}
                <a
                  class="btn"
                  data-variant="outline"
                  href={pageUrl(
                    "application",
                    data.Organization,
                    data.Space,
                    "app",
                    data.Application,
                    "tab",
                    "observability",
                    "subtab",
                    "logs",
                  )}>View logs</a
                >
                <a
                  class="btn"
                  data-variant="outline"
                  href={pageUrl(
                    "application",
                    data.Organization,
                    data.Space,
                    "app",
                    data.Application,
                    "tab",
                    "configuration",
                    "draft",
                    deployment.operation.planId,
                  )}>Review configuration</a
                >
              {/if}
              {#if deploymentIndex > 0}<a
                  class="btn"
                  data-variant="outline"
                  href={pageUrl(
                    "application",
                    data.Organization,
                    data.Space,
                    "app",
                    data.Application,
                    "tab",
                    "configuration",
                    "draft",
                    deployment.operation.planId,
                  )}>New version from this plan</a
                >{/if}
              {#if !terminal(deployment.operation.status) && deployment.operation.status !== "canceling"}<form
                  action="/api/ui/actions"
                  method="post"
                  use:submitForm={{}}
                >
                  <input
                    type="hidden"
                    name="action"
                    value="cancel-deployment"
                  /><input
                    type="hidden"
                    name="organization"
                    value={data.Organization}
                  /><input
                    type="hidden"
                    name="space"
                    value={data.Space}
                  /><input
                    type="hidden"
                    name="application"
                    value={data.Application}
                  /><input
                    type="hidden"
                    name="deployment"
                    value={deployment.operation.id}
                  /><button class="btn" data-variant="outline" type="submit"
                    >Cancel deployment</button
                  >
                </form>{/if}
            </div>
          </article>
        {/each}
      </div>
      <p class="field-help">
        A new version loads the saved settings into Form and YAML. Edit them,
        then review and deploy when ready.
      </p>{:else}<p class="empty-copy">
        Your first deployment will appear here.
      </p>{/if}
  </section>
</section>
