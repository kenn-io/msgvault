<script lang="ts">
  import type { Metrics } from '../../api/generated/models';

  let { metrics }: { metrics: Metrics } = $props();
  const headingID = $props.id();
  const basisLabels: Record<string, string> = {
    provider: 'Provider duration', scheduled: 'Scheduled', transcript_span: 'Transcript span', unknown: 'Unknown'
  };

  function duration(seconds: number | null): string {
    if (seconds === null) return 'Unavailable';
    const rounded = Math.round(seconds);
    const hours = Math.floor(rounded / 3600);
    const minutes = Math.floor((rounded % 3600) / 60);
    const remainder = rounded % 60;
    return [hours ? `${hours}h` : '', minutes ? `${minutes}m` : '', remainder ? `${remainder}s` : ''].filter(Boolean).join(' ') || '0s';
  }
</script>

<section class="meeting-metrics" aria-label="Meeting metrics">
  <header class="metrics-heading">
    <div>
      <span class="eyebrow">Overview</span>
      <h3>{metrics.totals.meeting_count.toLocaleString()} meetings</h3>
    </div>
    <p>{metrics.totals.known_duration_count.toLocaleString()} known · {metrics.totals.unknown_duration_count.toLocaleString()} unknown duration</p>
  </header>
  <dl class="metric-grid">
    <div>
      <dt>Total known meeting time</dt>
      <dd aria-label="Total known meeting time">{duration(metrics.totals.total_known_seconds)}</dd>
    </div>
    <div>
      <dt>Average known duration</dt>
      <dd aria-label="Average known duration">{duration(metrics.totals.average_known_seconds)}</dd>
    </div>
  </dl>
  <p class="note">Time includes provider duration, scheduled, and transcript span evidence where available. Unknown durations are excluded from the average.</p>
  {#if metrics.duration_by_basis.length > 0}
    <section class="evidence-section" aria-labelledby={`${headingID}-duration-evidence`}>
      <h4 id={`${headingID}-duration-evidence`}>Duration evidence</h4>
      <div class="table-scroll">
        <table aria-label="Duration evidence">
          <thead><tr><th scope="col">Evidence</th><th scope="col">Meetings</th><th scope="col">Known time</th></tr></thead>
          <tbody>{#each metrics.duration_by_basis as basis (basis.basis)}
            <tr><th scope="row">{basisLabels[basis.basis] ?? basis.basis}</th><td>{basis.count}</td><td>{basis.basis === 'unknown' ? 'Unavailable' : duration(basis.total_seconds)}</td></tr>
          {/each}</tbody>
        </table>
      </div>
    </section>
  {/if}
  {#if metrics.months.length > 0}
    <section class="evidence-section" aria-labelledby={`${headingID}-monthly`}>
      <h4 id={`${headingID}-monthly`}>Monthly activity</h4>
      <div class="table-scroll">
        <table aria-label="Monthly meeting activity">
          <thead><tr><th scope="col">Month</th><th scope="col">Meetings</th><th scope="col">Known</th><th scope="col">Unknown</th><th scope="col">Known time</th><th scope="col">Average</th></tr></thead>
          <tbody>{#each metrics.months as month (month.month)}
            <tr><th scope="row">{month.month}</th><td>{month.totals.meeting_count}</td><td>{month.totals.known_duration_count}</td><td>{month.totals.unknown_duration_count}</td><td>{duration(month.totals.total_known_seconds)}</td><td>{duration(month.totals.average_known_seconds)}</td></tr>
          {/each}</tbody>
        </table>
      </div>
      <p class="note">Months without meetings are omitted.</p>
    </section>
  {/if}
  {#if metrics.undated_count > 0}<p>{metrics.undated_count} meetings have no date and are excluded from monthly rows.</p>{/if}
</section>

<style>
  .meeting-metrics { display: grid; min-width: 0; gap: var(--space-4); }
  h3, h4, p, dl, dd { margin: 0; }
  .metrics-heading { display: flex; align-items: end; justify-content: space-between; gap: var(--space-4); }
  .metrics-heading > div { display: grid; gap: var(--space-1); }
  .eyebrow { color: var(--text-muted); font-size: var(--font-size-2xs); font-weight: 600; letter-spacing: .04em; text-transform: uppercase; }
  h3 { font-size: var(--font-size-md); color: var(--text-primary); }
  h4 { color: var(--text-primary); font-size: var(--font-size-sm); }
  p, dl { font-size: var(--font-size-sm); color: var(--text-secondary); }
  .metric-grid { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: var(--space-3); }
  .metric-grid > div { display: grid; gap: var(--space-2); padding: var(--space-3) var(--space-4); border-left: 2px solid var(--border-strong); background: var(--bg-inset); }
  dt { color: var(--text-muted); font-size: var(--font-size-xs); }
  dd { color: var(--text-primary); font-size: var(--font-size-lg); font-weight: 600; font-variant-numeric: tabular-nums; }
  .note { padding-top: var(--space-3); border-top: 1px solid var(--border-muted); color: var(--text-muted); font-size: var(--font-size-xs); line-height: 1.5; }
  .evidence-section { display: grid; gap: var(--space-2); padding-top: var(--space-3); border-top: 1px solid var(--border-muted); }
  .table-scroll { max-width: 100%; min-width: 0; overflow-x: auto; }
  table { width: 100%; border-collapse: collapse; font-size: var(--font-size-xs); }
  th, td { padding: var(--space-2); border-bottom: 1px solid var(--border-muted); text-align: right; }
  th:first-child { text-align: left; } th { color: var(--text-secondary); } td { color: var(--text-primary); }
  @media (max-width: 640px) {
    .metrics-heading { align-items: start; flex-direction: column; gap: var(--space-1); }
    .metric-grid { grid-template-columns: 1fr; }
  }
</style>
