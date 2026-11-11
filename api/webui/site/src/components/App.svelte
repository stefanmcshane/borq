<script lang="ts">
  import { onMount, tick } from "svelte";
  import Shell from "./Shell.svelte";
  import { request, envelopeSchema } from "../lib/api";
  import type { pageData } from "../lib/models";
  import { initializeControls } from "../lib/controls";

  let page: pageData | undefined;
  let error = "";
  let loading = false;
  let pending: AbortController | undefined;
  let revision = 0;
  let renderGeneration = 0;
  async function accept(next: pageData, push = false) {
    page = next;
    renderGeneration++;
    error = "";
    document.title = "Borq · " + next.Title;
    const address = next.Location || "/";
    if (push && address !== window.location.pathname + window.location.search)
      history.pushState({}, "", address);
    else history.replaceState({}, "", address);
    await tick();
    document.dispatchEvent(new CustomEvent("borq:render"));
  }
  async function navigate(address: string, push = false) {
    pending?.abort();
    pending = new AbortController();
    const version = ++revision;
    loading = true;
    try {
      const url = new URL(address, window.location.href);
      const result = await request("/api/ui/page" + url.search, {
        signal: pending.signal,
      });
      if (result.component !== "workspace")
        throw new Error("The server returned an unexpected workspace.");
      if (version === revision) await accept(result.data, push);
    } catch (cause) {
      if (version === revision && !pending.signal.aborted)
        error =
          cause instanceof Error
            ? cause.message
            : "Unable to load the workspace.";
    } finally {
      if (version === revision) loading = false;
    }
  }
  onMount(() => {
    const cleanup = initializeControls();
    const click = (event: MouseEvent) => {
      if (
        event.defaultPrevented ||
        event.button ||
        event.metaKey ||
        event.ctrlKey ||
        event.shiftKey ||
        event.altKey ||
        !(event.target instanceof Element)
      )
        return;
      const anchor = event.target.closest("a");
      if (
        !anchor ||
        anchor.target ||
        anchor.download ||
        anchor.getAttribute("href")?.startsWith("#")
      )
        return;
      const url = new URL(anchor.href, window.location.href);
      if (url.origin !== window.location.origin || url.pathname !== "/") return;
      event.preventDefault();
      void navigate(url.href, true);
    };
    const pop = () => {
      void navigate(window.location.href);
    };
    const workspace = (event: Event) => {
      if (!(event instanceof CustomEvent)) return;
      const parsed = envelopeSchema.safeParse(event.detail);
      if (parsed.success && parsed.data.component === "workspace") {
        revision++;
        pending?.abort();
        loading = false;
        void accept(parsed.data.data, true);
      }
    };
    document.addEventListener("click", click);
    document.addEventListener("borq:workspace", workspace);
    document.addEventListener("borq:refresh", pop);
    window.addEventListener("popstate", pop);
    void navigate(window.location.href);
    return () => {
      pending?.abort();
      cleanup();
      document.removeEventListener("click", click);
      document.removeEventListener("borq:workspace", workspace);
      document.removeEventListener("borq:refresh", pop);
      window.removeEventListener("popstate", pop);
    };
  });
</script>

{#if loading}<p
    id="refresh-progress"
    class="refresh-progress"
    role="status"
    data-loading
  >
    Loading…
  </p>{/if}
{#if page}<Shell data={page} {renderGeneration} />{:else}<main
    id="workspace"
    class="error-page"
  >
    <p role="status">Loading Borq workspace…</p>
  </main>{/if}
{#if error}<div class="error-page" role="alert">
    <p class="alert request-error">{error}</p>
    <button
      class="btn"
      type="button"
      onclick={() => navigate(window.location.href)}>Try again</button
    >
  </div>{/if}
