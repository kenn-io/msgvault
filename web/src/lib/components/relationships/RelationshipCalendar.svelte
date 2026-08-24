<script lang="ts">
  import type {
    RelationshipCalendar as RelationshipCalendarModel,
    RelationshipCalendarDay
  } from '../../relationships/models';

  interface Props {
    calendar: RelationshipCalendarModel | null;
    loading: boolean;
    error: string | null;
    year?: number;
    firstYear: number;
    currentYear: number;
    onYearChange: (year: number) => void;
  }

  interface CalendarCell {
    date: string;
    day?: RelationshipCalendarDay;
  }

  interface CalendarPanel {
    key: string;
    weeks: number;
    months: Array<{ label: string; week: number }>;
    cells: CalendarCell[];
  }

  let {
    calendar,
    loading,
    error,
    year = new Date().getUTCFullYear(),
    firstYear,
    currentYear,
    onYearChange
  }: Props = $props();

  const selectedYear = $derived(calendar?.year ?? year);
  const fullPanel = $derived(calendar ? buildCalendarPanel(calendar, 0, 11, 'full') : null);
  const halfPanels = $derived(calendar ? [
    buildCalendarPanel(calendar, 0, 5, 'first-half'),
    buildCalendarPanel(calendar, 6, 11, 'second-half')
  ] : []);
  const hasActivity = $derived(Boolean(calendar?.days?.some((day) => day.total > 0)));

  const weekdays = ['Sun', '', 'Tue', '', 'Thu', '', 'Sat'];
  const levels = ['none', 'first-quartile', 'second-quartile', 'third-quartile', 'fourth-quartile'];

  function buildCalendarPanel(
    response: RelationshipCalendarModel,
    startMonth: number,
    endMonth: number,
    key: string
  ): CalendarPanel {
    const start = new Date(Date.UTC(response.year, startMonth, 1));
    const end = new Date(Date.UTC(response.year, endMonth + 1, 0));
    const gridStart = addUTCDays(start, -start.getUTCDay());
    const gridEnd = addUTCDays(end, 6 - end.getUTCDay());
    const weeks = Math.floor((gridEnd.getTime() - gridStart.getTime()) / 604_800_000) + 1;
    const byDate = new Map((response.days ?? []).map((day) => [day.date, day]));
    const cells: CalendarCell[] = [];
    for (let week = 0; week < weeks; week += 1) {
      for (let weekday = 0; weekday < 7; weekday += 1) {
        const date = addUTCDays(gridStart, week * 7 + weekday);
        const outside = date < start || date > end;
        const dateString = formatUTCDate(date);
        cells.push({ date: dateString, day: outside ? undefined : byDate.get(dateString) });
      }
    }
    const months = [];
    for (let month = startMonth; month <= endMonth; month += 1) {
      const first = new Date(Date.UTC(response.year, month, 1));
      months.push({
        label: first.toLocaleString('en-US', { month: 'short', timeZone: 'UTC' }),
        week: Math.floor((first.getTime() - gridStart.getTime()) / 604_800_000)
      });
    }
    return { key, weeks, months, cells };
  }

  function addUTCDays(date: Date, days: number): Date {
    const copy = new Date(date);
    copy.setUTCDate(copy.getUTCDate() + days);
    return copy;
  }

  function formatUTCDate(date: Date): string {
    return date.toISOString().slice(0, 10);
  }

  function dayLabel(day: RelationshipCalendarDay): string {
    return `${day.date}: ${day.total} interactions; ${day.sent} sent, ${day.received} received, ` +
      `${day.email} email, ${day.chat} chat, ${day.meetings} meetings`;
  }

  function levelClass(day: RelationshipCalendarDay | undefined): string {
    return `level-${(day?.level ?? 'NONE').toLowerCase().replaceAll('_', '-')}`;
  }
</script>

{#snippet panel(panel: CalendarPanel, variant: 'full' | 'half')}
  <div class="calendar-panel {variant}" data-panel={panel.key}>
    <div class="month-row" style={`--weeks: ${panel.weeks}`} aria-hidden="true">
      {#each panel.months as month}
        <span style={`grid-column: ${month.week + 1} / span 2`}>{month.label}</span>
      {/each}
    </div>
    <div class="calendar-body">
      <div class="weekday-labels" aria-hidden="true">
        {#each weekdays as weekday}<span>{weekday}</span>{/each}
      </div>
      <div class="weeks" style={`--weeks: ${panel.weeks}`}>
        {#each panel.cells as cell (cell.date)}
          <span class:future={!cell.day} class="day">
            {#if cell.day}
              <button
                type="button"
                class="heat-cell {levelClass(cell.day)}"
                aria-label={dayLabel(cell.day)}
                title={dayLabel(cell.day)}
              ></button>
            {/if}
          </span>
        {/each}
      </div>
    </div>
  </div>
{/snippet}

<section class="relationship-calendar" aria-label="Relationship activity calendar">
  <div class="calendar-heading">
    <h2>Relationship</h2>
    <div class="year-navigation">
      <button
        type="button"
        aria-label="Previous relationship year"
        disabled={selectedYear <= firstYear || loading}
        onclick={() => onYearChange(selectedYear - 1)}
      >←</button>
      <span class="year">{selectedYear}</span>
      <button
        type="button"
        aria-label="Next relationship year"
        disabled={selectedYear >= currentYear || loading}
        onclick={() => onYearChange(selectedYear + 1)}
      >→</button>
    </div>
  </div>

  {#if loading && !calendar}
    <p class="calendar-state">Loading relationship activity…</p>
  {:else if error}
    <div class="calendar-state error" role="alert">
      <p>{error}</p>
      <button type="button" onclick={() => onYearChange(selectedYear)}>Retry</button>
    </div>
  {:else if calendar}
    <div class="calendar-graphs">
      {@render panel(fullPanel!, 'full')}
      <div class="split-panels">
        {#each halfPanels as half (half.key)}
          {@render panel(half, 'half')}
        {/each}
      </div>
    </div>
    {#if !hasActivity}<p class="empty-year">No interactions in {calendar.year}.</p>{/if}
    <div class="calendar-meta">
      <div class="legend" aria-label="Relationship activity intensity from less to more">
        <span>Less</span>
        {#each levels as level}<span class="heat-cell level-{level}" aria-hidden="true"></span>{/each}
        <span>More</span>
      </div>
      <div class="temperature-summary">
        <span>Current {calendar.current.temperature}/100</span>
        <span>Peak {calendar.peak_temperature}/100 - {calendar.peak_year}</span>
      </div>
    </div>
  {:else}
    <p class="calendar-state">Relationship activity has not loaded.</p>
  {/if}
</section>

<style>
  .relationship-calendar {
    --relationship-cell-size: 10px;
    --relationship-cell-gap: 3px;
    container-type: inline-size;
    flex: none;
    padding: var(--space-4);
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-md);
    background: var(--bg-surface);
  }

  .calendar-heading,
  .temperature-summary {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-3);
  }

  h2 {
    margin: 0;
    font-size: var(--text-sm);
    font-weight: 650;
  }

  .year-navigation {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }

  .year-navigation button {
    display: grid;
    width: 24px;
    height: 24px;
    place-items: center;
    padding: 0;
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-sm);
    color: var(--text-secondary);
    background: transparent;
    cursor: pointer;
  }

  .year-navigation button:disabled {
    opacity: 0.35;
    cursor: default;
  }

  .year {
    min-width: 4ch;
    text-align: center;
    font-variant-numeric: tabular-nums;
  }

  .calendar-graphs {
    max-width: 100%;
    margin-top: var(--space-3);
    overflow-x: auto;
  }

  .calendar-panel {
    width: max-content;
  }

  .month-row {
    display: grid;
    grid-template-columns: repeat(var(--weeks), var(--relationship-cell-size));
    gap: var(--relationship-cell-gap);
    margin-left: 30px;
    color: var(--text-muted);
    font-size: var(--text-xs);
  }

  .calendar-body {
    display: flex;
    gap: var(--space-2);
  }

  .weekday-labels {
    display: grid;
    width: 22px;
    flex: none;
    grid-template-rows: repeat(7, var(--relationship-cell-size));
    gap: var(--relationship-cell-gap);
    color: var(--text-muted);
    font-size: 9px;
    line-height: var(--relationship-cell-size);
  }

  .weeks {
    display: grid;
    grid-auto-flow: column;
    grid-template-rows: repeat(7, var(--relationship-cell-size));
    grid-auto-columns: var(--relationship-cell-size);
    gap: var(--relationship-cell-gap);
  }

  .day,
  .heat-cell {
    display: block;
    width: var(--relationship-cell-size);
    height: var(--relationship-cell-size);
    box-sizing: border-box;
    border-radius: 2px;
  }

  button.heat-cell {
    padding: 0;
    border: 0;
    cursor: default;
  }

  button.heat-cell:focus-visible {
    outline: 2px solid var(--accent-blue);
    outline-offset: 1px;
  }

  .level-none { background: #30363d; }
  .level-first-quartile { background: #6b4b16; }
  .level-second-quartile { background: #9a6818; }
  .level-third-quartile { background: #d18a18; }
  .level-fourth-quartile { background: #ffb000; }

  .split-panels {
    display: none;
    gap: var(--space-4);
  }

  .calendar-meta {
    display: grid;
    gap: var(--space-2);
    margin-top: var(--space-3);
  }

  .legend {
    display: flex;
    align-items: center;
    gap: 4px;
    color: var(--text-muted);
    font-size: var(--text-xs);
  }

  .legend .heat-cell {
    --relationship-cell-size: 9px;
  }

  .temperature-summary {
    font-size: var(--text-xs);
    font-variant-numeric: tabular-nums;
  }

  .calendar-state,
  .empty-year {
    margin: var(--space-3) 0 0;
    color: var(--text-muted);
    font-size: var(--text-sm);
  }

  .calendar-state {
    min-height: 92px;
  }

  .calendar-state.error { color: var(--text-danger); }

  @container (max-width: 700px) {
    .calendar-panel.full { display: none; }
    .split-panels { display: grid; }
  }
</style>
