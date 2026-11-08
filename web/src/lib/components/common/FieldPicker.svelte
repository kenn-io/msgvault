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

  // Kit names the open search by its placeholder, so the field writes the closed picker's words over it: opening the
  // picker doesn't change what it says, and the search keeps its name. An open panel ends at its last row, like a menu.
  function keepPlaceholder(node: HTMLElement): () => void {
    const fit = (): void => {
      const input = node.querySelector<HTMLInputElement>('.kit-typeahead__input');
      if (input && fallbackLabel && input.placeholder !== fallbackLabel) input.placeholder = fallbackLabel;
    };
    const observer = new MutationObserver(fit);
    observer.observe(node, { childList: true, subtree: true });
    return () => observer.disconnect();
  }
</script>

{#snippet errorRow()}<InlineNotice message={error} details={errorDetails} actionLabel={onretry ? 'Retry' : undefined} onaction={onretry} />{/snippet}
<div class="field-picker" role="group" aria-label={title} {@attach keepPlaceholder}>
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
  .creation { --creation-indent: calc(var(--space-box) + 2px); display: grid; gap: var(--space-pane); min-width: 0; padding-inline-start: var(--space-box); border-inline-start: 2px solid var(--border-muted); border-start-start-radius: 0; border-end-start-radius: 0; }
  /* A pair in the indented block splits where the form's pairs below it do: its first column gives up the indent. */
  @media (min-width: 641px) {
    .creation :global(.form-pair) { grid-template-columns: minmax(0, 1fr) calc((100% + var(--creation-indent) - var(--space-box)) / 2); }
  }
  .existing { justify-self: start; min-height: var(--control-h); padding: 0; border: 0; background: none; color: var(--link-ink); font: inherit; font-size: var(--type-meta-size); line-height: var(--type-meta-line); cursor: pointer; }
  .field-picker :global(.kit-typeahead__icon:not(:has(*))) { display: none; }
  .field-picker :global(.kit-typeahead__empty:empty) { display: none; }
  /* A panel holding only its error ends at the error, 4 below it as above: no empty list, and no divider over one.
   * (Chrome drops a :has() nested in another, so the panel, not the header, asks whether the list shows anything.) */
  .field-picker :global(.kit-typeahead__body:not(:has(> :not(.kit-typeahead__empty:empty)))) { display: none; }
  .field-picker :global(.kit-typeahead__panel:not(:has(.kit-typeahead__body > :not(.kit-typeahead__empty:empty))) > .kit-typeahead__header) { margin-bottom: 0; padding-bottom: 0; border-bottom: 0; }
</style>
