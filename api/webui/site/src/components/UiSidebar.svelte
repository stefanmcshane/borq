<script lang="ts">
  import UiIcon from "./UiIcon.svelte";
  import UiNavigation from "./UiNavigation.svelte";
  import type { pageData } from "../lib/models";
  export let data: pageData;
</script>

<aside class="app-sidebar" aria-label="Workspace navigation">
  <a class="workspace-brand" href="/" aria-label="Borq home"
    ><img src="/assets/borq.svg" width="26" height="26" alt="" /><span
      >Borq<span class="workspace-brand-caption">Workspace</span></span
    ></a
  >
  <nav id="sidebar-navigation" class="sidebar-nav" aria-label="Main navigation">
    <UiNavigation {data} />
  </nav>
  <div class="sidebar-bottom">
    <div class="sidebar-account-actions">
      <a class="btn" data-variant="ghost" href="/account">Account and API tokens</a>
      <form method="post" action="/api/auth/logout">
        <button class="btn" data-variant="ghost" type="submit">Sign out</button>
      </form>
    </div>
    <div class="sidebar-workspace-row">
      <div class="workspace-identity">
        <span class="workspace-avatar" aria-hidden="true">B</span><span
          >Borq<span class="workspace-caption">Workspace</span></span
        >
      </div>
      <button
        class="btn theme-toggle"
        data-variant="ghost"
        data-size="icon-sm"
        data-theme-toggle
        type="button"
        aria-label="Switch to light theme"
        title="Switch to light theme"
        ><span class="theme-sun"><UiIcon data={"sun"} /></span><span
          class="theme-moon"><UiIcon data={"moon"} /></span
        ></button
      >
    </div>
  </div>
</aside>
