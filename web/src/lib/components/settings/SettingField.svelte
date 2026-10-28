<script lang="ts" module>
  const optionLabels: Readonly<Record<string, string>> = {
    openai: 'OpenAI-compatible',
    'voyage-contextual': 'Voyage contextual',
    duckdb: 'DuckDB',
    sql: 'SQL',
    'sqlite-vec': 'sqlite-vec',
    pgvector: 'pgvector',
  };

  function optionLabel(value: string): string {
    if (Object.hasOwn(optionLabels, value)) return optionLabels[value];
    if (value === '') return 'Default';
    const words = value.replaceAll('_', ' ');
    return words.charAt(0).toUpperCase() + words.slice(1);
  }
</script>

<script lang="ts">
  import { SelectDropdown, TextInput, Toggle } from '@kenn-io/kit-ui';
  import { valueText, type SettingState } from '../../settings/catalog';
  import CronField from './CronField.svelte';

  type SettingOff = NonNullable<NonNullable<SettingState['validation']>['off']>;

  let {
    setting,
    disabled = false,
    label,
    value,
    onchange,
    optionLabels = undefined,
  }: {
    disabled?: boolean;
    setting: SettingState;
    label: string;
    /** The draft when one exists, otherwise the stored value. */
    value: unknown;
    onchange: (value: unknown) => void;
    /** Names for option values that would otherwise read as raw codes. */
    optionLabels?: Readonly<Record<string, string>>;
  } = $props();

  const text = $derived(valueText(value));
  const off = $derived(setting.validation?.off);
  const switchedOff = $derived(off !== undefined && text === (off.value ?? ''));
  const options = $derived(setting.options ?? []);

  // A setting with an off value renders as a switch beside its control:
  // off stores the daemon's off value, on starts from the suggested value.
  function switchTo(offValue: SettingOff, on: boolean) {
    const numeric = setting.kind === 'integer' || setting.kind === 'number';
    if (!on) {
      onchange(numeric ? Number(offValue.value ?? '') : (offValue.value ?? ''));
      return;
    }
    if (numeric) {
      const suggested = offValue.suggest === undefined || offValue.suggest === '' ? Number.NaN : Number(offValue.suggest);
      onchange(Number.isNaN(suggested) ? (offValue.on_minimum ?? setting.validation?.minimum ?? 1) : suggested);
      return;
    }
    onchange(offValue.suggest ?? '');
  }
</script>

{#if options.length > 0}
  <label class="field" data-size="md">
    <span class="kit-sr-only">{label}</span>
    <SelectDropdown
      {disabled}
      value={text}
      title={label}
      options={options.map((option) => ({ value: option, label: optionLabels?.[option] ?? optionLabel(option) }))}
      onchange={(next) => onchange(next)}
    />
  </label>
{:else if setting.kind === 'boolean'}
  <Toggle {disabled} ariaLabel={label} checked={Boolean(value)} onchange={(checked) => onchange(checked)} />
{:else}
  {#if off}
    <Toggle {disabled} ariaLabel={`Set ${label}`} checked={!switchedOff} onchange={(on) => switchTo(off, on)} />
  {/if}
  {#if off && switchedOff}
    <span class="off">{off.label}</span>
  {:else if setting.validation?.format === 'cron'}
    <div class="field" data-size="cron">
      <CronField {disabled} {label} value={text} required={setting.validation?.required} oninput={(next) => onchange(next)} />
    </div>
  {:else if setting.kind === 'integer' || setting.kind === 'number'}
    <label class="field" data-size="xs">
      <span class="kit-sr-only">{label}</span>
      <input
        {disabled}
        type="number"
        data-mono
        value={text}
        step={setting.kind === 'integer' ? '1' : 'any'}
        min={off?.on_minimum ?? setting.validation?.minimum}
        max={setting.validation?.maximum}
        required={setting.validation?.required || off !== undefined}
        aria-invalid={value === '' || undefined}
        oninput={(event) => {
          const raw = event.currentTarget.value;
          onchange(raw === '' ? '' : Number(raw));
        }}
      />
    </label>
  {:else}
    <label class="field" data-size={off ? 'sm' : 'lg'}>
      <span class="kit-sr-only">{label}</span>
      <TextInput
        {disabled}
        value={text}
        block
        oninput={(next) =>
          onchange(
            setting.kind === 'string_array'
              ? next
                  .split(',')
                  .map((item) => item.trim())
                  .filter(Boolean)
              : next,
          )}
      />
    </label>
  {/if}
{/if}

<style>
  .field {
    display: block;
    min-width: 0;
  }
  .field[data-size='xs'] {
    width: 6.5rem;
  }
  /* Values beside a switch are short sizes or durations such as 512MiB. */
  .field[data-size='sm'] {
    width: 9rem;
  }
  .field[data-size='md'] {
    width: 13rem;
  }
  .field[data-size='lg'] {
    width: 15rem;
  }
  /* A schedule is one line: presets, the expression when custom, and the zone. */
  .field[data-size='cron'] {
    width: 100%;
    max-width: 30rem;
  }
  .off {
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }
  input[type='number'] {
    box-sizing: border-box;
    width: 100%;
    height: var(--control-height);
    padding: 0 var(--space-3);
    border: var(--border-width) solid var(--border-default);
    border-radius: var(--radius-md);
    background: var(--bg-surface);
    color: var(--text-primary);
    font: inherit;
    font-size: var(--font-size-sm);
    transition: border-color var(--transition-fast) var(--transition-ease, ease);
  }
  input[type='number']:focus-visible {
    outline: var(--focus-ring);
    outline-offset: 1px;
    border-color: var(--accent-blue);
  }
</style>
