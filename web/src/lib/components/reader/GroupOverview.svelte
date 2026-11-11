<script lang="ts">
  import type { ReadingPaneSelection } from './ReadingPane.svelte';
  import { bucketSeries } from '../../explore/group-activity';
  import { groupRowName } from '../../explore/labels';
  import { formatBytes, formatDate } from '../../util/format';

  let { selection }: {
    selection: Extract<ReadingPaneSelection, { kind: 'group' }>;
  } = $props();
  const series = $derived(bucketSeries(selection.overview?.activity ?? []));
  const first = $derived(series[0]);
  const last = $derived(series.at(-1));
  const busiest = $derived(series.reduce((best, month) => month.count > best.count ? month : best, { bucket: '', count: 0 }));
  const daily = $derived(first?.bucket.length === 10);
  const monthName = (bucket: string) => bucket.length === 10
    ? new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeZone: 'UTC' }).format(new Date(bucket))
    : groupRowName('month', bucket, bucket).name;
  const stripLabel = $derived(first && last
    ? series.length === 1
      ? `Activity by ${daily ? 'day' : 'month'}: ${monthName(first.bucket)}, ${first.count.toLocaleString()} ${first.count === 1 ? 'item' : 'items'}`
      : `Activity by ${daily ? 'day' : 'month'}, ${monthName(first.bucket)} to ${monthName(last.bucket)}, busiest ${monthName(busiest.bucket)} with ${busiest.count.toLocaleString()} ${busiest.count === 1 ? 'item' : 'items'}`
    : 'Activity');
</script>

<section class="group-overview" aria-label="Group overview">
  <div class="count-line">
    <strong>{selection.count?.toLocaleString() ?? '—'} {selection.count === 1 ? 'item' : 'items'}</strong>
    {#if selection.estimatedBytes !== undefined}<span>{formatBytes(selection.estimatedBytes)}</span>{/if}
  </div>
  {#if selection.overview?.first_at && selection.latestAt && !(daily && formatDate(selection.overview.first_at) === monthName(first!.bucket) && formatDate(selection.latestAt) === monthName(last!.bucket))}
    {@const firstDate = formatDate(selection.overview.first_at)}
    {@const lastDate = formatDate(selection.latestAt)}
    <p>{firstDate}{firstDate === lastDate ? '' : ` – ${lastDate}`}</p>
  {/if}
  {#if series.length > 0 && first && last}
    <div class="activity">
      <svg role="img" aria-label={stripLabel} viewBox={`0 0 ${series.length * 8 - 2} 32`} preserveAspectRatio="none">
        {#each series as month, index (month.bucket)}
          {@const height = busiest.count ? month.count / busiest.count * 32 : 0}
          <rect x={index * 8} y={32 - height} width="6" {height} />
        {/each}
      </svg>
      <div class="month-labels"><span>{monthName(first.bucket)}</span>{#if series.length > 1}<span>{monthName(last.bucket)}</span>{/if}</div>
    </div>
  {/if}
  {#if selection.overview?.co_participants?.length}
    <div class="people">
      <h2>Most often with</h2>
      {#each selection.overview?.co_participants as person (person.key)}
        <div class="person"><span>{person.label}</span><span data-mono>{person.count.toLocaleString()}</span></div>
      {/each}
    </div>
  {/if}
</section>

<style>
  .group-overview { display: grid; gap: var(--space-4); padding: var(--pane-pad-y) var(--pane-pad-x); border-bottom: 1px solid var(--border-muted); }
  .count-line, .person, .month-labels { display: flex; align-items: baseline; justify-content: space-between; gap: var(--space-4); }
  .count-line strong { color: var(--text-primary); font-size: var(--font-size-lg); font-weight: 600; }
  .count-line span, p { color: var(--text-muted); font-size: var(--font-size-sm); }
  p, h2 { margin: 0; }
  svg { display: block; width: 100%; height: 32px; fill: var(--accent-blue); }
  .activity, .people { display: grid; gap: var(--space-2); }
  .people { padding-top: var(--space-4); border-top: 1px solid var(--border-muted); }
  .month-labels, h2 { color: var(--text-muted); font-size: var(--font-size-2xs); }
  h2 { font-weight: 600; letter-spacing: .04em; text-transform: uppercase; }
  .person { color: var(--text-secondary); font-size: var(--font-size-sm); }
</style>
