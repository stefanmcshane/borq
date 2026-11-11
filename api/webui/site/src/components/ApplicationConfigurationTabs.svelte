<script lang="ts">
  import { truth, pageUrl, yaml, variant } from "../lib/format";
  import type { applicationEditorData } from "../lib/models";
  export let data: applicationEditorData;
</script>

<nav class="application-subtabs" aria-label="Configuration editor">
  {#if data.Format === "manifests"}
    <a
      href={pageUrl(
        "application",
        data.Organization,
        data.Space,
        "app",
        data.ExistingName,
        "tab",
        "configuration",
        "subtab",
        "fields",
      )}>Form</a
    >
    <a
      href={pageUrl(
        "application",
        data.Organization,
        data.Space,
        "app",
        data.ExistingName,
        "tab",
        "configuration",
        "subtab",
        "yaml",
      )}>YAML</a
    >
  {:else}
    <button
      class="btn"
      data-variant="ghost"
      type="submit"
      name="editorFormat"
      value="fields"
      formaction="/api/ui/application-editor"
      formnovalidate
      aria-current={truth(data.Format === "fields") ? "page" : undefined}
      >Form</button
    >
    <button
      class="btn"
      data-variant="ghost"
      type="submit"
      name="editorFormat"
      value="yaml"
      formaction="/api/ui/application-editor"
      formnovalidate
      aria-current={truth(data.Format === "yaml") ? "page" : undefined}
      >YAML</button
    >
  {/if}
  {#if truth(data.ExistingName)}<a
      href={pageUrl(
        "application",
        data.Organization,
        data.Space,
        "app",
        data.ExistingName,
        "tab",
        "configuration",
        "subtab",
        "manifests",
      )}
      aria-current={truth(data.Format === "manifests") ? "page" : undefined}
      >Kubernetes Manifests</a
    >{/if}
</nav>
