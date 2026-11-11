<script lang="ts">
  import UiIcon from "./UiIcon.svelte";
  import { truth } from "../lib/format";
  import type { applicationEditorData } from "../lib/models";
  export let data: applicationEditorData;
</script>

<fieldset class="application-section application-routes">
  <legend
    ><UiIcon data={"globe"} /> Public routes
    <span class="badge" data-variant="secondary">HTTPS</span></legend
  >
  <p class="field-help">
    Choose one subdomain for a domain allowed by this space’s cluster
    certificate issuer. HTTPS certificates are managed automatically.
  </p>
  {#if data.RoutePolicy.certificateIssuer}
    <p class="field-help">
      Certificate issuer: <code>{data.RoutePolicy.certificateIssuer}</code> (managed
      by this space’s cluster)
    </p>
  {/if}
  {#if data.RoutePolicyError}
    <p class="field-help" role="alert">
      {data.RoutePolicyError} Internal applications can still be edited; retry before
      changing public routes.
    </p>
  {:else if data.RoutePolicy.domains.length === 0}
    <p class="field-help" role="status">
      No public route domains are configured for this space’s cluster. Configure
      its certificate issuer before adding a public route.
    </p>
  {/if}
  <input type="hidden" name="ingressFields" value="on" />
  <div class="application-route-list">
    {#each data.Ingresses as ingress, ingressIndex}
      <div
        class="application-route"
        role="group"
        aria-label={"Public route " +
          (truth(ingress.Route.name) ? ingress.Route.name : "draft")}
      >
        <div class="application-route-heading">
          <span
            >{#if truth(ingress.Route.name)}{ingress.Route.name}{:else}New route{/if}</span
          >{#if truth(ingress.Saved)}<span class="badge" data-variant="outline"
              >Deployed</span
            >{:else}<button
              class="btn"
              data-variant="destructive"
              type="submit"
              name="removeIngress"
              value={ingressIndex}
              formaction="/api/ui/application-editor"
              formnovalidate
              aria-label={"Remove route " +
                (truth(ingress.Route.name) ? ingress.Route.name : "draft")}
              ><UiIcon data={"trash"} /> Remove</button
            >{/if}
        </div>
        <div class="application-route-fields">
          <label
            >Route name<input
              class="input"
              name="ingressName"
              value={ingress.Route.name}
              readonly={truth(ingress.Saved)}
              data-focus={ingress.Focus ? "" : undefined}
              required
              maxlength="63"
              pattern={"[a-z0-9]([a-z0-9\\-]*[a-z0-9])?"}
              placeholder="public"
              spellcheck="false"
            /></label
          >
          <label
            >Subdomain<input
              class="input mono"
              name="ingressSubdomain"
              aria-label="Subdomain"
              value={ingress.Subdomain}
              required
              maxlength="63"
              pattern={"[a-z0-9]([a-z0-9\\-]*[a-z0-9])?"}
              placeholder="web"
              spellcheck="false"
              aria-invalid={ingress.HostError ? "true" : undefined}
            />
            {#if data.RoutePolicy.domains.length > 1}
              <select
                class="input mono"
                name="ingressDomain"
                aria-label="Route domain"
                value={ingress.Domain}
              >
                {#each data.RoutePolicy.domains as domain}<option value={domain}
                    >.{domain}</option
                  >{/each}
              </select>
            {:else}
              <input
                type="hidden"
                name="ingressDomain"
                value={ingress.Domain}
              />
              <span class="field-help mono">.{ingress.Domain}</span>
            {/if}
            {#if ingress.HostError}<span class="field-help" role="alert"
                >{ingress.HostError}</span
              >{/if}
          </label>
          <label
            >Path prefix<input
              class="input mono"
              name="ingressPath"
              value={truth(ingress.Route.path) ? ingress.Route.path : "/"}
              required
              pattern="/.*"
              placeholder="/"
              spellcheck="false"
            /></label
          >
        </div>
      </div>
    {:else}
      <div
        class="application-route-empty"
        data-internal-access
        data-namespace={data.Namespace}
      >
        <UiIcon data={"globe"} />
        <div>
          <strong data-internal-title
            >{#if data.Draft.port === 0}No network service{:else}Internal access
              only{/if}</strong
          >
          <code
            class="internal-address"
            data-internal-address
            hidden={!(data.Namespace && data.Draft.name && data.Draft.port > 0)}
            >{#if truth(data.Namespace) && truth(data.Draft.name) && truth(data.Draft.port > 0)}http://{data
                .Draft.name}.{data.Namespace}.svc:{data.Draft.port}{/if}</code
          >
          <p data-internal-help>
            {#if data.Draft.port === 0}Set a container port to give this app an
              internal address.{:else if truth(truth(data.Namespace) && truth(data.Draft.name))}Available
              after deployment to apps in this cluster. Add a public route for
              HTTPS access.{:else}Enter an application name to preview its
              internal address.{/if}
          </p>
        </div>
      </div>
    {/each}
  </div>
  <div class="application-route-footer">
    <button
      class="btn"
      data-variant="outline"
      type="submit"
      name="editorAction"
      value="add-ingress"
      formaction="/api/ui/application-editor"
      disabled={data.RoutePolicy.domains.length === 0}
      formnovalidate><UiIcon data={"plus"} /> Add route</button
    >
    <span class="field-help"
      >Requires a nonzero container port.{#if truth(data.SavedIngresses)}
        Deployed routes can be updated; removal is not supported yet.{/if}</span
    >
  </div>
</fieldset>
