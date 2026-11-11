<script lang="ts">
  import { truth } from "../lib/format";
  import type { usageChart } from "../lib/models";
  export let data: usageChart;
</script>

{#key data}
  <section class="card usage-card">
    <header>
      <h3>{data.Title}</h3>
      <span class="field-help">{data.Unit}</span>
    </header>
    {#if truth(data.Samples)}<div class="usage-reading">
        <strong>{data.Latest}</strong><span
          >{data.Unit} <small>latest recorded</small></span
        >
      </div>{:else}<p class="field-help chart-no-samples">
        No metrics yet. New workloads may take a couple of minutes to receive
        samples.
      </p>{/if}
    <div
      class="chart-stage"
      data-usage-chart
      data-unit={data.Unit}
      data-step={data.Step}
    >
      <span class="chart-ceiling">{data.Maximum}</span><svg
        class="usage-chart"
        viewBox="0 0 600 180"
        role="slider"
        aria-valuemin="0"
        aria-valuemax="600"
        aria-valuenow="600"
        tabindex="0"
        aria-label={data.Title +
          " over the selected range; values in " +
          data.Unit +
          ". Use left and right arrows to inspect measurements, up and down arrows for events."}
        ><path class="chart-grid" d="M0 10H600 M0 85H600 M0 160H600" />
        {#each data.Segments as segment}<polyline
            class="chart-line"
            points={segment}
          />{/each}
        {#each data.Samples as sample}<circle
            class="chart-sample"
            cx={sample.X}
            cy={sample.Y}
            r="0"
            data-chart-point
            data-time={sample.Time}
            data-value={sample.Value}
          />{/each}
        {#each data.Events as event}<g
            class="chart-event"
            data-kind={event.Kind}
            data-chart-event
            data-x={event.X}
            data-time={event.Time}
            data-clock={event.Clock}
            data-message={event.Message}
            ><line x1={event.X} x2={event.X} y1="10" y2="160" /><circle
              cx={event.X}
              cy="9"
              r="3"
            /><title>{event.Time} · {event.Message}</title></g
          >{/each}
        <g class="chart-cursor" visibility="hidden"
          ><line y1="10" y2="160" /><circle r="4" /></g
        ></svg
      >
      <div class="chart-tooltip" role="status" aria-live="polite" hidden></div>
      <div class="chart-times">
        <span>{data.Start}</span><span>{data.End}</span>
      </div>
    </div>
    {#if truth(data.Samples)}<footer>
        <span>Average <strong>{data.Average} {data.Unit}</strong></span><span
          >Peak <strong>{data.Peak} {data.Unit}</strong></span
        >
      </footer>
      <details class="chart-data">
        <summary>View measurements</summary>
        <div class="chart-data-scroll">
          <table class="table">
            <thead><tr><th>Time</th><th>{data.Unit}</th></tr></thead><tbody
              >{#each data.Samples as sample}<tr
                  ><td>{sample.Time}</td><td>{sample.Value}</td></tr
                >{/each}</tbody
            >
          </table>
        </div>
      </details>{/if}
    {#if truth(data.Events)}<details class="chart-data">
        <summary>View {data.Events.length} timeline events</summary>
        <ol class="chart-event-list">
          {#each data.Events as event}<li>
              <time>{event.Time}</time>
              {event.Message}
            </li>{/each}
        </ol>
      </details>{/if}
  </section>
{/key}
