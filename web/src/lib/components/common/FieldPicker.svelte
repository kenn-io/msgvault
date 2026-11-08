<script lang="ts">
  import { Typeahead, type TypeaheadOption } from '@kenn-io/kit-ui';
  import type { Snippet } from 'svelte';
  import InlineNotice from './InlineNotice.svelte';

  let { options, value, onselect, placeholder, fallbackLabel = '', title = placeholder,
    loading = false, loadingLabel, error = '', errorDetails, onretry, onquery, emptyLabel = 'No matches',
    oncreate, createLabel, existingLabel, creation, creating = false, onexisting,
    icon, panelAnchor, disabled = false, remote = false, highlightFirstOption = false }: {
    options: TypeaheadOption[]; value: string; onselect: (value: string) => void | Promise<void>;
    placeholder: string; fallbackLabel?: string; title?: string; loading?: boolean; loadingLabel?: string;
    error?: string; errorDetails?: string; onretry?: () => void; onquery?: (query: string) => void; emptyLabel?: string;
    oncreate?: (query: string) => void; createLabel?: string; existingLabel?: string; creation?: Snippet; creating?: boolean;
    onexisting?: () => void; icon?: Snippet<[TypeaheadOption]>; panelAnchor?: HTMLElement;
    disabled?: boolean; remote?: boolean; highlightFirstOption?: boolean;
  } = $props();
</script>

{#snippet errorRow()}<InlineNotice message={error} details={errorDetails} actionLabel={onretry ? 'Retry' : undefined} onaction={onretry} />{/snippet}
<div class="field-picker" role="group" aria-label={title}>
  <Typeahead {options} {value} {placeholder} {fallbackLabel} {title} disabled={disabled || creating}
    {loading} {loadingLabel} {panelAnchor} {highlightFirstOption} {icon}
    {remote} emptyLabel={error ? '' : emptyLabel} allowCustom={!!oncreate} customLabel={createLabel}
    {onquery} onselect={async name => { if (oncreate && !options.some(option => option.name === name)) oncreate(name); else await onselect(name); }}
    header={error && !loading ? errorRow : undefined} />
  {#if creating}<div class="creation">
    {@render creation?.()}
    {#if onexisting && existingLabel}<button class="existing kit-control-states" type="button" onclick={onexisting}>{existingLabel}</button>{/if}
  </div>{/if}
</div>

<style>
  .field-picker { display: grid; gap: var(--space-pane); min-width: 0; --typeahead-max-width: none; --typeahead-min-width: 0px; }
  .creation { display: grid; gap: var(--space-pane); min-width: 0; padding-inline-start: var(--space-box); border-inline-start: 2px solid var(--border-muted); border-start-start-radius: 0; border-end-start-radius: 0; }
  .existing { justify-self: start; min-height: var(--control-h); padding: 0; border: 0; background: none; color: var(--link-ink); font: inherit; font-size: var(--type-meta-size); line-height: var(--type-meta-line); cursor: pointer; }
  .field-picker :global(.kit-typeahead__icon:not(:has(*))) { display: none; }
  .field-picker :global(.kit-typeahead__empty:empty) { display: none; }
</style>
