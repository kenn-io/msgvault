<script lang="ts">
  import type { APIClient } from '../../api/client';
  import { createExploreAPI } from '../../explore/api';
  import type { RelationshipCalendar as Calendar } from '../../relationships/models';
  import RelationshipCalendar from '../relationships/RelationshipCalendar.svelte';

  let { client, domain, firstAt, lastAt, selectedDate, onDateChange, year = $bindable<number | null>(null) }:
    { client: APIClient; domain: string; firstAt: string; lastAt: string; selectedDate: string | null; year?: number | null; onDateChange: (date: string | null) => void } = $props();
  const currentYear = new Date().getFullYear();
  let retry = $state(0);
  let calendar = $state<(Pick<Calendar, 'year' | 'days'> & Partial<Calendar>) | null>(null);
  let loading = $state(false);
  let error = $state<string | null>(null);
  const firstYear = $derived(new Date(firstAt).getFullYear() || currentYear);
  const since = $derived(firstAt ? new Intl.DateTimeFormat('en-US', { month: 'short', year: 'numeric', timeZone: Intl.DateTimeFormat().resolvedOptions().timeZone }).format(new Date(firstAt)) : undefined);
  $effect(() => { void domain; year ??= new Date(lastAt).getFullYear() || currentYear; calendar = null; });
  $effect(() => {
    void retry;
    const wanted = year ?? currentYear;
    const abort = new AbortController();
    loading = true;
    error = null;
    calendar = null;
    void createExploreAPI(client).groups({ filters: [
      { dimension: 'domain', values: [domain] },
      { dimension: 'after', values: [new Date(wanted, 0, 1).toISOString()] },
      { dimension: 'before', values: [new Date(wanted + 1, 0, 1).toISOString()] },
    ], timezone: Intl.DateTimeFormat().resolvedOptions().timeZone, sent_only: true, limit: 366, presentation: 'table' }, 'day', abort.signal).then((result) => {
      if (abort.signal.aborted) return;
      if (result.status !== 'ready') { error = result.unavailable.message; return; }
      const counts = result.result.rows;
      const levels = ['FIRST_QUARTILE', 'SECOND_QUARTILE', 'THIRD_QUARTILE', 'FOURTH_QUARTILE'] as const;
      const byDate = new Map(counts.map((row) => [row.key, row.count]));
      const days: Calendar['days'] = [];
      const end = Math.min(new Date(wanted + 1, 0, 1).getTime(), Date.now());
      for (const at = new Date(wanted, 0, 1); at.getTime() < end; at.setDate(at.getDate() + 1)) {
        const date = `${at.getFullYear()}-${String(at.getMonth() + 1).padStart(2, '0')}-${String(at.getDate()).padStart(2, '0')}`;
        const count = byDate.get(date) ?? 0;
        days.push({ date, total: count, email: count, chat: 0, meetings: 0, sent: 0, received: 0, modality_mask: 0,
          level: count === 0 ? 'NONE' : levels[Math.max(0, Math.ceil(counts.filter((other) => other.count <= count).length / counts.length * 4) - 1)]!,
        });
      }
      calendar = { year: wanted, days };

    }).catch((cause: unknown) => { if (!abort.signal.aborted) error = cause instanceof Error ? cause.message : 'Could not load activity.'; })
      .finally(() => { if (!abort.signal.aborted) loading = false; });
    return () => abort.abort();
  });
</script>

<RelationshipCalendar {calendar} {loading} {error} year={year ?? currentYear} {firstYear} {currentYear} {selectedDate} {since}
  title="Activity" showTemperature={false} {onDateChange}
  onYearChange={(next) => { onDateChange(null); year = next; retry += 1; }} />
