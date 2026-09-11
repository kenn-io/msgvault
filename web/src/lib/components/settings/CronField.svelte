<script module lang="ts">
  import type { TypeaheadOption } from '@kenn-io/kit-ui';
  import { timeZoneLabel, timeZoneNames } from '../../settings/cron';

  let zoneOptionsCache: TypeaheadOption[] | undefined;

  // The zone list is the same for every field on the page, so build it once.
  function zoneOptions(): TypeaheadOption[] {
    zoneOptionsCache ??= timeZoneNames().map((zone) => ({
      name: zone,
      label: timeZoneLabel(zone),
      meta: zoneOffset(zone),
    }));
    return zoneOptionsCache;
  }

  function zoneOffset(zone: string): string | undefined {
    try {
      return new Intl.DateTimeFormat('en-US', { timeZone: zone, timeZoneName: 'shortOffset' })
        .formatToParts(new Date())
        .find((part) => part.type === 'timeZoneName')?.value;
    } catch {
      return undefined;
    }
  }
</script>

<script lang="ts">
  import { SelectDropdown, Typeahead } from '@kenn-io/kit-ui';
  import {
    CRON_FIELDS,
    CRON_PRESETS,
    describeFields,
    joinCron,
    parseCron,
    splitCron,
    type CronToken,
  } from '../../settings/cron';

  interface Segment {
    text: string;
    field?: CronToken['field'];
    invalid?: boolean;
  }

  let {
    value = $bindable(''),
    label,
    required = false,
    disabled = false,
    id = undefined,
    oninput = undefined,
  }: {
    /** The stored schedule, with a `CRON_TZ=` prefix when a zone is chosen. */
    value?: string;
    /** Accessible name for the expression input. */
    label: string;
    /** When false, an empty expression means the schedule is off. */
    required?: boolean;
    disabled?: boolean;
    id?: string;
    oninput?: (value: string) => void;
  } = $props();

  const uid = $props.id();
  const statusID = `${uid}-status`;
  // The zone and the five fields are edited separately but stored as one
  // string. Remember the last split this field produced so typing keeps its
  // exact text and a zone survives while the expression is empty.
  let local = $state<{ source: string; zone: string; expression: string }>();
  const parts = $derived(local !== undefined && local.source === value ? local : splitCron(value));
  const parsed = $derived(parseCron(value));
  const tokens = $derived(parseCron(parts.expression).tokens);
  const empty = $derived(parts.expression.trim() === '');
  const invalid = $derived(empty ? required : parsed.fields === undefined);
  let mirror = $state<HTMLDivElement>();
  const segments = $derived(segmentsOf(parts.expression, tokens));
  const status = $derived.by((): { tone: 'off' | 'error' | 'ok'; text: string } => {
    if (empty) {
      return required
        ? { tone: 'error', text: 'Enter a schedule.' }
        : { tone: 'off', text: 'Off. Nothing runs on a schedule.' };
    }
    if (parsed.fields) return { tone: 'ok', text: describeFields(parsed.fields, parsed.zone) };
    return { tone: 'error', text: parsed.error ?? 'Invalid schedule.' };
  });
  const presetOptions = $derived([
    ...(required ? [] : [{ value: 'off', label: 'Off' }]),
    ...CRON_PRESETS.map((preset) => ({ value: preset.expression, label: preset.label })),
  ]);
  // Choosing Custom, or typing in the editor, keeps the editor open even
  // while the text equals a preset, so a schedule can be edited through a
  // preset's text without the editor closing mid-keystroke. The latch is
  // tied to the value it was set for: a value replaced from outside (Discard,
  // a reload, a conflict) shows whatever preset matches the new value.
  let customMode = $state(false);
  const customActive = $derived(customMode && local !== undefined && local.source === value);
  const presetValue = $derived.by(() => {
    if (empty) return required ? 'custom' : 'off';
    if (customActive) return 'custom';
    const normalized = tokens.map((token) => token.text).join(' ');
    return CRON_PRESETS.some((preset) => preset.expression === normalized) ? normalized : 'custom';
  });
  const presetMenu = $derived([...presetOptions, { value: 'custom', label: 'Custom' }]);
  const summary = $derived(parsed.fields ? describeFields(parsed.fields) : '');
  // A stored zone the browser's list lacks (a legacy alias, or a newer zone
  // database on the daemon) still shows on the trigger and stays selectable.
  const zoneMenu = $derived(
    parts.zone !== '' && !zoneOptions().some((option) => option.name === parts.zone)
      ? [{ name: parts.zone, label: timeZoneLabel(parts.zone) }, ...zoneOptions()]
      : zoneOptions(),
  );

  function segmentsOf(expression: string, expressionTokens: CronToken[]): Segment[] {
    const result: Segment[] = [];
    let cursor = 0;
    for (const token of expressionTokens) {
      if (token.start > cursor) result.push({ text: expression.slice(cursor, token.start) });
      result.push({ text: token.text, field: token.field, invalid: token.error !== undefined });
      cursor = token.end;
    }
    if (cursor < expression.length) result.push({ text: expression.slice(cursor) });
    return result;
  }

  function update(zone: string, expression: string) {
    const next = joinCron(zone, expression);
    local = { source: next, zone, expression };
    value = next;
    oninput?.(next);
  }

  function applyPreset(next: string) {
    if (next === 'custom') {
      customMode = true;
      if (empty) update(parts.zone, '0 3 * * *');
      else local = { source: value, zone: parts.zone, expression: parts.expression };
      return;
    }
    customMode = false;
    update(parts.zone, next === 'off' ? '' : next);
  }
</script>

<div class="cron" class:cron--disabled={disabled}>
  <div class="cron__row">
    <div class="cron__presets">
      <SelectDropdown title="Presets" value={presetValue} options={presetMenu} onchange={applyPreset} {disabled} />
    </div>
    {#if presetValue === 'custom'}
      <div class="cron__editor" class:cron__editor--invalid={invalid}>
        <div class="cron__mirror" aria-hidden="true" bind:this={mirror}>
          {#each segments as segment, index (index)}
            {#if segment.field}
              <span data-field={segment.field} data-invalid={segment.invalid || undefined}>{segment.text}</span>
            {:else}
              {segment.text}
            {/if}
          {/each}
        </div>
        <input
          class="cron__input"
          type="text"
          {id}
          aria-label={label}
          aria-invalid={invalid || undefined}
          aria-describedby={statusID}
          autocomplete="off"
          autocapitalize="off"
          spellcheck="false"
          placeholder="0 3 * * *"
          {required}
          {disabled}
          value={parts.expression}
          oninput={(event) => {
            customMode = true;
            update(parts.zone, event.currentTarget.value);
          }}
          onscroll={(event) => {
            if (mirror) mirror.scrollLeft = event.currentTarget.scrollLeft;
          }}
        />
        <!-- Field names and the plain-English reading appear above the
             editor only while it is hovered or focused, so the row stays one
             line and nothing below gets covered. -->
        <span class="cron__legend kit-popover-card" aria-hidden="true">
          <span class="cron__legend-fields">
            {#each CRON_FIELDS as field (field.name)}
              <span data-field={field.name}>{field.label}</span>
            {/each}
          </span>
          {#if summary}<span class="cron__summary">{summary}</span>{/if}
        </span>
      </div>
    {/if}
    {#if !empty}
      <div class="cron__zone">
        <Typeahead
          options={zoneMenu}
          value={parts.zone}
          fallbackLabel="Server time"
          placeholder="Time zone"
          title="Time zone"
          emptyLabel="No matching time zone"
          allowClear
          clearLabel="Server time"
          {disabled}
          onselect={(zone) => update(zone, parts.expression)}
        />
      </div>
    {/if}
  </div>
  <p class="cron__status" class:kit-sr-only={status.tone !== 'error'} id={statusID} data-tone={status.tone}>
    {status.text}
  </p>
</div>

<style>
  .cron {
    display: grid;
    gap: var(--space-1);
    min-width: 0;
    /* Accents darkened toward the text color, the same recipe as the app's
       status ink tokens, so small tinted text clears 4.5:1 in both themes. */
    --cron-minute: color-mix(in srgb, var(--accent-blue) 72%, var(--text-primary));
    --cron-hour: color-mix(in srgb, var(--accent-teal) 72%, var(--text-primary));
    --cron-day: color-mix(in srgb, var(--accent-green) 72%, var(--text-primary));
    --cron-month: color-mix(in srgb, var(--accent-amber) 72%, var(--text-primary));
    --cron-weekday: color-mix(in srgb, var(--accent-purple) 72%, var(--text-primary));
  }
  .cron__editor {
    position: relative;
    box-sizing: border-box;
    min-width: 0;
    height: 28px;
    border: var(--border-width) solid var(--border-default);
    border-radius: var(--radius-md);
    background: var(--bg-surface);
    transition: border-color var(--transition-fast) var(--transition-ease, ease);
  }
  .cron__editor:focus-within {
    border-color: var(--accent-blue);
    outline: var(--focus-ring);
    outline-offset: 1px;
  }
  .cron__editor--invalid {
    border-color: var(--accent-red);
  }
  .cron__mirror,
  .cron__input {
    box-sizing: border-box;
    width: 100%;
    height: 100%;
    margin: 0;
    padding: 0 var(--space-3);
    border: 0;
    font-family: var(--font-mono);
    font-size: var(--font-size-sm);
    line-height: 26px;
    letter-spacing: 0.02em;
    white-space: pre;
    overflow: hidden;
  }
  .cron__mirror {
    position: absolute;
    inset: 0;
    color: var(--text-primary);
    pointer-events: none;
  }
  .cron__mirror [data-field='minute'] {
    color: var(--cron-minute);
  }
  .cron__mirror [data-field='hour'] {
    color: var(--cron-hour);
  }
  .cron__mirror [data-field='day'] {
    color: var(--cron-day);
  }
  .cron__mirror [data-field='month'] {
    color: var(--cron-month);
  }
  .cron__mirror [data-field='weekday'] {
    color: var(--cron-weekday);
  }
  .cron__mirror [data-field='extra'],
  .cron__mirror [data-invalid] {
    color: var(--status-error-ink);
    text-decoration: underline wavy;
    text-underline-offset: 3px;
  }
  .cron__input {
    position: relative;
    background: transparent;
    color: transparent;
    caret-color: var(--text-primary);
  }
  .cron__input::placeholder {
    color: var(--text-muted);
  }
  .cron__input:focus {
    outline: none;
  }
  .cron__input:disabled {
    cursor: not-allowed;
  }
  .cron--disabled .cron__editor {
    opacity: 0.6;
  }
  /* One line where it fits (the settings row gives it 30rem); the zone
     wraps under the presets on a narrow screen instead of running off it. */
  .cron__row {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-2);
    min-width: 0;
  }
  .cron__presets {
    flex: 0 0 9.5rem;
    min-width: 0;
  }
  .cron__editor {
    flex: 1 1 7rem;
    min-width: 6.5rem;
  }
  /* Fixed width, so opening the menu (which swaps the button for a search
     box) moves nothing beside it. Zone names are long; the list opens wider
     than the trigger and the toolkit keeps it inside the window. */
  .cron__zone {
    flex: 0 0 11rem;
    --typeahead-min-width: 0;
    --typeahead-max-width: none;
    --typeahead-panel-min-width: 18rem;
  }
  .cron__legend {
    position: absolute;
    left: 0;
    bottom: calc(100% + 4px);
    z-index: 1;
    display: grid;
    gap: 2px;
    padding: var(--space-2) var(--space-3);
    font-family: var(--font-mono);
    font-size: var(--font-size-2xs);
    font-weight: 600;
    letter-spacing: 0.02em;
    white-space: nowrap;
    visibility: hidden;
  }
  .cron__legend-fields {
    display: flex;
    gap: var(--space-3);
  }
  .cron__summary {
    font-family: inherit;
    font-size: var(--font-size-xs);
    font-weight: 400;
    letter-spacing: 0;
    color: var(--text-secondary);
  }
  .cron__editor:hover .cron__legend,
  .cron__editor:focus-within .cron__legend {
    visibility: visible;
  }
  .cron--disabled .cron__editor:hover .cron__legend {
    visibility: hidden;
  }
  .cron__legend [data-field='minute'] {
    color: var(--cron-minute);
  }
  .cron__legend [data-field='hour'] {
    color: var(--cron-hour);
  }
  .cron__legend [data-field='day'] {
    color: var(--cron-day);
  }
  .cron__legend [data-field='month'] {
    color: var(--cron-month);
  }
  .cron__legend [data-field='weekday'] {
    color: var(--cron-weekday);
  }
  .cron__status {
    margin: 0;
    color: var(--text-secondary);
    font-size: var(--font-size-xs);
    line-height: 1.4;
  }
  .cron__status[data-tone='off'] {
    color: var(--text-muted);
  }
  .cron__status[data-tone='error'] {
    color: var(--status-error-ink);
  }
</style>
