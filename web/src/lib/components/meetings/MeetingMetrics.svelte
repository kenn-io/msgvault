<script lang="ts">
  import type { Metrics } from '../../api/generated/models';

  let { metrics }: { metrics: Metrics } = $props();
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
  <h3>{metrics.totals.meeting_count.toLocaleString()} meetings</h3>
  <p>{metrics.totals.known_duration_count.toLocaleString()} known · {metrics.totals.unknown_duration_count.toLocaleString()} unknown duration</p>
  <dl>
    <div><dt>Total known meeting time</dt><dd aria-label="Total known meeting time">{duration(metrics.totals.total_known_seconds)}</dd></div>
    <div><dt>Average known duration</dt><dd aria-label="Average known duration">{duration(metrics.totals.average_known_seconds)}</dd></div>
  </dl>
  <p class="note">Time includes provider duration, scheduled, and transcript span evidence where available. Unknown durations are excluded from the average.</p>
  {#if metrics.duration_by_basis.length > 0}
    <div class="table-scroll">
      <table aria-label="Duration evidence">
        <thead><tr><th scope="col">Evidence</th><th scope="col">Meetings</th><th scope="col">Known time</th></tr></thead>
        <tbody>{#each metrics.duration_by_basis as basis (basis.basis)}
          <tr><th scope="row">{basisLabels[basis.basis] ?? basis.basis}</th><td>{basis.count}</td><td>{basis.basis === 'unknown' ? 'Unavailable' : duration(basis.total_seconds)}</td></tr>
        {/each}</tbody>
      </table>
    </div>
  {/if}
  {#if metrics.months.length > 0}
    <div class="table-scroll">
      <table aria-label="Monthly meeting activity">
        <thead><tr><th scope="col">Month</th><th scope="col">Meetings</th><th scope="col">Known</th><th scope="col">Unknown</th><th scope="col">Known time</th><th scope="col">Average</th></tr></thead>
        <tbody>{#each metrics.months as month (month.month)}
          <tr><th scope="row">{month.month}</th><td>{month.totals.meeting_count}</td><td>{month.totals.known_duration_count}</td><td>{month.totals.unknown_duration_count}</td><td>{duration(month.totals.total_known_seconds)}</td><td>{duration(month.totals.average_known_seconds)}</td></tr>
        {/each}</tbody>
      </table>
    </div>
    <p class="note">Months without meetings are omitted.</p>
  {/if}
  {#if metrics.undated_count > 0}<p>{metrics.undated_count} meetings have no date and are excluded from monthly rows.</p>{/if}
</section>

<style>
  .meeting-metrics { display: grid; gap: var(--space-3); }
  h3, p, dl, dd { margin: 0; }
  h3 { font-size: var(--font-size-sm); color: var(--text-primary); }
  p, dl { font-size: var(--font-size-sm); color: var(--text-secondary); }
  dl { display: flex; flex-wrap: wrap; gap: var(--space-4); }
  dd { color: var(--text-primary); font-variant-numeric: tabular-nums; }
  .note { color: var(--text-muted); font-size: var(--font-size-xs); }
  .table-scroll { overflow-x: auto; }
  table { width: 100%; border-collapse: collapse; font-size: var(--font-size-xs); }
  th, td { padding: var(--space-2); border-bottom: 1px solid var(--border-muted); text-align: right; }
  th:first-child { text-align: left; } th { color: var(--text-secondary); } td { color: var(--text-primary); }
</style>
