<script lang="ts">
  import { SelectDropdown } from '@kenn-io/kit-ui';
  import { CRON_FIELDS, CRON_PRESETS, describeFields, parseCron, type CronToken } from '../../settings/cron';

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
  const parsed = $derived(parseCron(value));
  const empty = $derived(value.trim() === '');
  const invalid = $derived(empty ? required : parsed.fields === undefined);
  let mirror = $state<HTMLDivElement>();
  const segments = $derived(segmentsOf(value, parsed.tokens));
  const status = $derived.by((): { tone: 'off' | 'error' | 'ok'; text: string } => {
    if (empty) {
      return required
        ? { tone: 'error', text: 'Enter a schedule.' }
        : { tone: 'off', text: 'Off. Nothing runs on a schedule.' };
    }
    if (parsed.fields) return { tone: 'ok', text: describeFields(parsed.fields) };
    return { tone: 'error', text: parsed.error ?? 'Invalid schedule.' };
  });
  const presetOptions = $derived([
    ...(required ? [] : [{ value: 'off', label: 'Off' }]),
    ...CRON_PRESETS.map((preset) => ({ value: preset.expression, label: preset.label })),
  ]);
  const presetValue = $derived.by(() => {
    if (empty) return required ? 'custom' : 'off';
    const normalized = parsed.tokens.map((token) => token.text).join(' ');
    return CRON_PRESETS.some((preset) => preset.expression === normalized) ? normalized : 'custom';
  });
  const presetMenu = $derived(
    presetValue === 'custom' ? [{ value: 'custom', label: 'Custom' }, ...presetOptions] : presetOptions,
  );

  function segmentsOf(expression: string, tokens: CronToken[]): Segment[] {
    const result: Segment[] = [];
    let cursor = 0;
    for (const token of tokens) {
      if (token.start > cursor) result.push({ text: expression.slice(cursor, token.start) });
      result.push({ text: token.text, field: token.field, invalid: token.error !== undefined });
      cursor = token.end;
    }
    if (cursor < expression.length) result.push({ text: expression.slice(cursor) });
    return result;
  }

  function applyPreset(next: string) {
    if (next === 'custom') return;
    value = next === 'off' ? '' : next;
    oninput?.(value);
  }
</script>

<div class="cron" class:cron--disabled={disabled}>
  <div class="cron__row">
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
        bind:value
        oninput={() => oninput?.(value)}
        onscroll={(event) => {
          if (mirror) mirror.scrollLeft = event.currentTarget.scrollLeft;
        }}
      />
    </div>
    <SelectDropdown
      title="Presets"
      value={presetValue}
      options={presetMenu}
      onchange={applyPreset}
      {disabled}
      align="end"
    />
  </div>
  <div class="cron__legend" aria-hidden="true">
    {#each CRON_FIELDS as field (field.name)}
      <span data-field={field.name}>{field.label}</span>
    {/each}
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
  .cron__row {
    display: flex;
    align-items: center;
    gap: var(--space-3);
  }
  .cron__editor {
    position: relative;
    flex: 1 1 auto;
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
  .cron__legend {
    display: flex;
    gap: var(--space-4);
    padding: 0 var(--space-3);
    font-size: var(--font-size-2xs);
    font-weight: 600;
    letter-spacing: 0.02em;
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
