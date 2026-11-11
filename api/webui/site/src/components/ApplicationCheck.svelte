<script lang="ts">
  import { truth } from "../lib/format";
  import type { applicationCheckData } from "../lib/models";
  export let data: applicationCheckData;
</script>

<div class="application-probe">
  <label class="checkbox-label application-probe-toggle"
    ><input
      class="checkbox"
      type="checkbox"
      name={data.Name}
      checked={truth(data.Check.enabled)}
    /><span>{data.Title} check<small>{data.Description}</small></span></label
  >
  <div class="form-grid two-columns">
    <label
      >HTTP path<input
        class="input mono"
        name={data.Name + "Path"}
        value={data.Check.path}
        required
        pattern="/.*"
        placeholder="/healthz"
        spellcheck="false"
      /></label
    >
    <label
      >Probe port<input
        class="input"
        type="number"
        name={data.Name + "Port"}
        min="0"
        max="65535"
        {...truth(data.PortSet)
          ? { required: true, value: data.Check.port }
          : {}}
        placeholder="Container port"
      /><small
        >{#if truth(data.PortSet)}Independent of the container port. Use 0 only
          when disabled.{:else}Leave blank to use the container port.{/if}</small
      ></label
    >
  </div>
  <details class="application-probe-timing">
    <summary>Timing and failure policy</summary>
    <div class="form-grid">
      <label
        >Interval (seconds)<input
          class="input"
          type="number"
          name={data.Name + "PeriodSeconds"}
          min="1"
          max="2147483647"
          required
          value={data.Check.periodSeconds}
        /><small>Time between probe attempts.</small></label
      >
      <label
        >Timeout (seconds)<input
          class="input"
          type="number"
          name={data.Name + "TimeoutSeconds"}
          min="1"
          max="2147483647"
          required
          value={data.Check.timeoutSeconds}
        /><small>Maximum duration of each attempt.</small></label
      >
      <label
        >Failure threshold<input
          class="input"
          type="number"
          name={data.Name + "FailureThreshold"}
          min="1"
          max="2147483647"
          required
          value={data.Check.failureThreshold}
        /><small>Consecutive failures before the check fails.</small></label
      >
    </div>
  </details>
</div>
