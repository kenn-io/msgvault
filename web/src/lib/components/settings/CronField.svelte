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
  const presetValue = $derived.by(() => {
    if (empty) return required ? 'custom' : 'off';
    const normalized = tokens.map((token) => token.text).join(' ');
    return CRON_PRESETS.some((preset) => preset.expression === normalized) ? normalized : 'custom';
  });
  const presetMenu = $derived(
    presetValue === 'custom' ? [{ value: 'custom', label: 'Custom' }, ...presetOptions] : presetOptions,
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
    if (next === 'custom') return;
    update(parts.zone, next === 'off' ? '' : next);
  }
</script>

<div class="cron" class:cron--disabled={disabled}>
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
      oninput={(event) => update(parts.zone, event.currentTarget.value)}
      onscroll={(event) => {
        if (mirror) mirror.scrollLeft = event.currentTarget.scrollLeft;
      }}
    />
    <!-- Field names appear above the editor only while it is hovered or
         focused, so the row stays quiet and nothing below gets covered. -->
    <span class="cron__legend kit-popover-card" aria-hidden="true">
      {#each CRON_FIELDS as field (field.name)}
        <span data-field={field.name}>{field.label}</span>
      {/each}
    </span>
  </div>
  <div class="cron__menus">
    <SelectDropdown title="Presets" value={presetValue} options={presetMenu} onchange={applyPreset} {disabled} />
    <div class="cron__zone">
      <Typeahead
        options={zoneOptions()}
        value={parts.zone}
        fallbackLabel="Local time"
        placeholder="Time zone"
        title="Time zone"
        triggerPrefix="Time zone:"
        emptyLabel="No matching time zone"
        allowClear
        clearLabel="Local time"
        disabled={disabled || empty}
        onselect={(zone) => update(zone, parts.expression)}
      />
    </div>
  </div>
  <p class="cron__status" id={statusID} data-tone={status.tone}>{status.text}</p>
</div>

<style>
  .cron {
    display: grid;
    gap: var(--space-2);
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
  .cron__menus {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    min-width: 0;
  }
  .cron__zone {
    flex: 1 1 auto;
    min-width: 0;
    --typeahead-min-width: 0;
    --typeahead-max-width: none;
  }
  .cron__legend {
    position: absolute;
    left: 0;
    bottom: calc(100% + 4px);
    z-index: 1;
    display: flex;
    gap: var(--space-3);
    padding: 2px var(--space-3);
    font-family: var(--font-mono);
    font-size: var(--font-size-2xs);
    font-weight: 600;
    letter-spacing: 0.02em;
    white-space: nowrap;
    visibility: hidden;
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
