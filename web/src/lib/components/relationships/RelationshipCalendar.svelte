<script lang="ts">
  import ChevronLeftIcon from '@lucide/svelte/icons/chevron-left';
  import ChevronRightIcon from '@lucide/svelte/icons/chevron-right';
  import { Button, Card, IconButton } from '@kenn-io/kit-ui';

  import type {
    RelationshipCalendar as RelationshipCalendarModel,
    RelationshipCalendarDay
  } from '../../relationships/models';
  import { dayTooltipText } from '../../relationships/calendar-tooltip';

  interface Props {
    calendar: RelationshipCalendarModel | null;
    loading: boolean;
    error: string | null;
    year?: number;
    firstYear: number | null;
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
  const multiYear = $derived(firstYear === null || firstYear < currentYear);
  let root = $state<HTMLElement>();
  const tooltipID = $props.id();
  let tooltip = $state<HTMLElement | null>(null);
  let tooltipNode = $state<HTMLElement>();
  let activeCell: HTMLElement | null = null;
  let pointer: { x: number; y: number } | null = null;
  // Survives the pointer being cleared (focus moves, hide) so scroll
  // dismissal still knows what kind of pointer was last seen.
  let lastPointerType: string | null = null;

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

  $effect(() => {
    calendar;
    hideTooltip();
  });

  $effect(() => {
    const cell = tooltip;
    if (!cell || !tooltipNode) return;
    positionTooltip(cell);
    cell.setAttribute('aria-describedby', tooltipID);
    document.addEventListener('keydown', dismissTooltip, true);
    return () => {
      cell.removeAttribute('aria-describedby');
      document.removeEventListener('keydown', dismissTooltip, true);
    };
  });

  function positionTooltip(target: HTMLElement): void {
    if (!root || !tooltipNode) return;
    const cell = target.getBoundingClientRect();
    const box = root.getBoundingClientRect();
    const halfWidth = tooltipNode.getBoundingClientRect().width / 2;
    const center = cell.left - box.left + cell.width / 2;
    tooltipNode.style.left = `${Math.max(halfWidth + 4, Math.min(center, box.width - halfWidth - 4))}px`;
    tooltipNode.style.top = `${cell.top - box.top}px`;
  }

  function activateTooltip(target: HTMLElement): void {
    if (target === activeCell) return;
    activeCell = target;
    tooltip = target;
  }

  function dismissTooltip(event: KeyboardEvent): void {
    if (event.key !== 'Escape') return;
    event.preventDefault();
    event.stopPropagation();
    tooltip = null;
    pointer = null;
    // Keep activeCell until the pointer leaves so movement within a dismissed
    // day does not reopen it. A different day can open immediately.
  }

  function showTooltip(event: Event): void {
    if (event.type.startsWith('pointer')) {
      const movement = event as PointerEvent;
      pointer = { x: movement.clientX, y: movement.clientY };
      lastPointerType = movement.pointerType;
    } else if (event.type === 'focusin') {
      pointer = null;
    }
    const target = (event.target as HTMLElement | null)?.closest<HTMLElement>('button.heat-cell');
    if (target) activateTooltip(target);
    else hideTooltip();
  }

  function hideTooltip(): void {
    activeCell = null;
    tooltip = null;
    // Drop captured coordinates so a later scroll cannot resurrect the tip
    // from wherever the pointer last was.
    pointer = null;
  }

  function tooltipPointerLeave(event: PointerEvent): void {
    // Touch pointers fire pointerleave right after pointerup; keep a tapped
    // day's tooltip up until the next tap or scroll elsewhere.
    if (event.pointerType === 'touch') return;
    hideTooltip();
  }

  function scrollTooltip(): void {
    if (!root || !tooltip) return;
    // A touch drag leaves the tap behind (its coordinates may already be
    // cleared by focus); scrolling must dismiss the tooltip rather than
    // re-pin it to an unrelated cell.
    if (lastPointerType === 'touch') {
      hideTooltip();
      return;
    }
    const pointedCell = pointer
      ? document.elementFromPoint(pointer.x, pointer.y)?.closest<HTMLElement>('button.heat-cell')
      : activeCell;
    if (!pointedCell || !root.contains(pointedCell)) {
      hideTooltip();
      return;
    }
    if (pointedCell !== activeCell) {
      activateTooltip(pointedCell);
      return;
    }
    positionTooltip(pointedCell);
  }

  function levelClass(day: RelationshipCalendarDay | undefined): string {
    return `level-${(day?.level ?? 'NONE').toLowerCase().replaceAll('_', '-')}`;
  }
</script>

{#snippet panel(panel: CalendarPanel, variant: 'full' | 'half')}
  <div class="calendar-panel {variant}" data-panel={panel.key} style={`--weeks: ${panel.weeks}`}>
    <div class="month-row" aria-hidden="true">
      {#each panel.months as month}
        <span style={`grid-column: ${month.week + 1}`}>{month.label}</span>
      {/each}
    </div>
    <div class="calendar-body">
      <div class="weekday-labels" aria-hidden="true">
        {#each weekdays as weekday}<span>{weekday}</span>{/each}
      </div>
      <div class="weeks" role="group" aria-label="Calendar days" onpointerover={showTooltip} onpointermove={showTooltip} onfocusin={showTooltip} onpointerleave={tooltipPointerLeave} onfocusout={hideTooltip}>
        {#each panel.cells as cell (cell.date)}
          <span class:future={!cell.day} class="day">
            {#if cell.day}
              <button
                type="button"
                class="heat-cell {levelClass(cell.day)}"
                aria-label={dayTooltipText(cell.day)}
              ></button>
            {/if}
          </span>
        {/each}
      </div>
    </div>
  </div>
{/snippet}

<Card padding="sm">
<section class="relationship-calendar" aria-label="Relationship activity calendar" bind:this={root}>
  <div class="calendar-heading">
    <h2>Relationship</h2>
    <div class="year-navigation">
      {#if multiYear}
        <IconButton
          ariaLabel="Previous relationship year"
          disabled={firstYear === null || selectedYear <= firstYear || loading}
          onclick={() => onYearChange(selectedYear - 1)}
        ><ChevronLeftIcon size="14" aria-hidden="true" /></IconButton>
      {/if}
      <span class="year">{selectedYear}</span>
      {#if multiYear}
        <IconButton
          class="next-year"
          ariaLabel="Next relationship year"
          disabled={firstYear === null || selectedYear >= currentYear || loading}
          onclick={() => onYearChange(selectedYear + 1)}
        ><ChevronRightIcon size="14" aria-hidden="true" /></IconButton>
      {/if}
    </div>
  </div>

  {#if loading && !calendar}
    <p class="calendar-state">Loading relationship activity…</p>
  {:else if error}
    <div class="calendar-state error" role="alert">
      <p>{error}</p>
      <Button label="Retry" onclick={() => onYearChange(selectedYear)} />
    </div>
  {:else if calendar}
    <div class="calendar-graphs" onscroll={scrollTooltip}>
      {@render panel(fullPanel!, 'full')}
      <div class="split-panels">
        {#each halfPanels as half (half.key)}
          {@render panel(half, 'half')}
        {/each}
      </div>
    </div>
    {#if tooltip}
      <!-- kit-ui-check-ignore: One delegated tooltip follows individual days; kit Tooltip owns its trigger and open state. -->
      <div id={tooltipID} class="calendar-day-tooltip kit-popover-card" role="tooltip" bind:this={tooltipNode}>
        {tooltip.getAttribute('aria-label')}
      </div>
    {/if}
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
</Card>

<style>
  .relationship-calendar {
    --relationship-cell-gap: 3px;
    --relationship-label-gutter: calc(22px + var(--space-2));
    container-type: inline-size;
    position: relative;
    flex: none;
    min-width: 0;
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
    display: grid;
    grid-template-columns: var(--kit-control-height, 28px) 4ch var(--kit-control-height, 28px);
    align-items: center;
    gap: var(--space-2);
  }

  .year-navigation :global(.next-year) {
    grid-column: 3;
  }

  .year {
    grid-column: 2;
    grid-row: 1;
    min-width: 4ch;
    text-align: center;
    font-variant-numeric: tabular-nums;
  }

  .calendar-graphs {
    max-width: 100%;
    margin-top: var(--space-3);
    overflow-x: auto;
    overflow-y: hidden;
  }

  .calendar-panel {
    width: 100%;
    min-width: calc(var(--relationship-label-gutter) + var(--weeks) * 8px + (var(--weeks) - 1) * var(--relationship-cell-gap));
    --relationship-cell-size: max(8px, calc((100cqi - var(--relationship-label-gutter) - (var(--weeks) - 1) * var(--relationship-cell-gap)) / var(--weeks)));
  }

  .month-row {
    display: grid;
    grid-template-columns: repeat(var(--weeks), var(--relationship-cell-size));
    gap: var(--relationship-cell-gap);
    margin-left: var(--relationship-label-gutter);
    color: var(--text-muted);
    font-size: var(--text-xs);
  }

  .month-row span {
    white-space: nowrap;
    overflow: visible;
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

  .level-none { background: var(--bg-inset); }
  .level-first-quartile { background: color-mix(in srgb, var(--accent-amber) 22%, var(--bg-inset)); }
  .level-second-quartile { background: color-mix(in srgb, var(--accent-amber) 42%, var(--bg-inset)); }
  .level-third-quartile { background: color-mix(in srgb, var(--accent-amber) 68%, var(--bg-inset)); }
  .level-fourth-quartile { background: var(--accent-amber); }

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
    flex-wrap: wrap;
    font-size: var(--text-xs);
    font-variant-numeric: tabular-nums;
  }

  .calendar-day-tooltip {
    position: absolute;
    z-index: var(--z-tooltip);
    width: max-content;
    transform: translate(-50%, calc(-100% - 6px));
    max-width: calc(100% - 8px);
    box-sizing: border-box;
    padding: 4px 8px;
    color: var(--text-primary);
    font-size: var(--font-size-sm);
    line-height: 1.45;
    pointer-events: none;
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
