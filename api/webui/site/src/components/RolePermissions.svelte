<script lang="ts">
  export let available: string[];
  export let selected: string[];
</script>

<fieldset class="form-grid two-columns">
  <legend>Permissions</legend>
  {#each available as permission}
    <label class="checkbox-label">
      <input
        class="checkbox"
        type="checkbox"
        name="permissions"
        value={permission}
        checked={selected.includes(permission)}
      />
      <span class="mono">{permission}</span>
    </label>
  {/each}
</fieldset>
