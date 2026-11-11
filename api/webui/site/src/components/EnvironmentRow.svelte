<script lang="ts">
  import type { ApiVariable } from "../lib/models";
  export let data: ApiVariable;
</script>

<div
  class="environment-row"
  data-environment-row
  role="group"
  aria-label="Environment variable"
>
  <label
    >Key<input
      class="input mono"
      name="variableName"
      value={data.name}
      required
      placeholder="LOG_LEVEL"
      spellcheck="false"
    /></label
  >
  <label
    >Value<textarea
      class="textarea environment-value mono"
      name="variableValue"
      rows="1"
      wrap="off"
      placeholder="info"
      spellcheck="false"
      value={data.value}></textarea></label
  >
  <button
    class="environment-text-action environment-remove"
    type="button"
    data-environment-remove>Remove variable</button
  >
</div>
