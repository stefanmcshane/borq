<script lang="ts">
  import { truth, terminal } from "../lib/format";
  export let data: string;
</script>

<svg
  class="ui-icon"
  width="16"
  height="16"
  viewBox="0 0 24 24"
  fill="none"
  stroke="currentColor"
  stroke-width="1.6"
  stroke-linecap="round"
  stroke-linejoin="round"
  aria-hidden="true"
>
  {#if data === "overview"}<rect
      x="3"
      y="3"
      width="7"
      height="7"
      rx="1.5"
    /><rect x="14" y="3" width="7" height="7" rx="1.5" /><rect
      x="3"
      y="14"
      width="7"
      height="7"
      rx="1.5"
    /><rect x="14" y="14" width="7" height="7" rx="1.5" />
  {:else if data === "applications"}<path
      d="m12 3 8 4.5v9L12 21l-8-4.5v-9L12 3Z"
    /><path d="m4 7.5 8 4.7 8-4.7M12 12v9" />
  {:else if data === "spaces"}<rect
      x="3"
      y="5"
      width="18"
      height="15"
      rx="2"
    /><path d="M8 5V3m8 2V3M3 10h18M8 10v10" />
  {:else if data === "clusters"}<rect
      x="3"
      y="3"
      width="18"
      height="7"
      rx="2"
    /><rect x="3" y="14" width="18" height="7" rx="2" /><path
      d="M7 6.5h.01M7 17.5h.01M11 6.5h6M11 17.5h6"
    />
  {:else if data === "database"}<ellipse cx="12" cy="5" rx="8" ry="3" /><path
      d="M4 5v14c0 1.7 3.6 3 8 3s8-1.3 8-3V5M4 12c0 1.7 3.6 3 8 3s8-1.3 8-3"
    />
  {:else if data === "management"}<rect
      x="5"
      y="3"
      width="14"
      height="18"
      rx="2"
    /><path d="M9 8h6M9 12h6M9 16h4" />
  {:else if data === "globe"}<circle cx="12" cy="12" r="9" /><path
      d="M3 12h18M12 3a18 18 0 0 1 0 18 18 18 0 0 1 0-18Z"
    />
  {:else if data === "health"}<path d="M3 12h4l3-7 4 14 3-7h4" />
  {:else if data === "terminal"}<rect
      x="3"
      y="4"
      width="18"
      height="16"
      rx="2"
    /><path d="m7 9 3 3-3 3m6 0h4" />
  {:else if data === "plus"}<path d="M12 5v14M5 12h14" />
  {:else if data === "trash"}<path
      d="M3 6h18M9 6V4h6v2m-10 0 1 14h12l1-14M10 10v6m4-6v6"
    />
  {:else if data === "chevron-down"}<path d="m7 10 5 5 5-5" />
  {:else if data === "refresh"}<path d="M20 7v5h-5M4 17v-5h5" /><path
      d="M6.1 8a7 7 0 0 1 11.5-2L20 9M4 15l2.4 3A7 7 0 0 0 17.9 16"
    />
  {:else if data === "close"}<path d="m6 6 12 12M6 18 18 6" />
  {:else if data === "check"}<circle cx="12" cy="12" r="9" /><path
      d="m8 12 3 3 5-6"
    />
  {:else if data === "warning"}<path d="m12 3 10 18H2L12 3Z" /><path
      d="M12 9v4m0 4h.01"
    />
  {:else if data === "sun"}<circle cx="12" cy="12" r="4" /><path
      d="M12 2v2m0 16v2M2 12h2m16 0h2M5 5l1.4 1.4m11.2 11.2L19 19M5 19l1.4-1.4M17.6 6.4 19 5"
    />
  {:else if data === "moon"}<path
      d="M20.5 13.1A8.5 8.5 0 0 1 10.9 3a8.5 8.5 0 1 0 9.6 10.1Z"
    />
  {/if}</svg
>
