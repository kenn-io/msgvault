<script lang="ts">
  import { autoReposition, Button, Checkbox, escapeCloses } from '@kenn-io/kit-ui';
  import { onDestroy, tick, untrack, type Snippet } from 'svelte';

  import type { APIClient } from '../../api/client';
  import { boundaryDate, editedBoundary, isLocalBound } from '../../explore/date-bound';
  import { createExploreAPI } from '../../explore/api';
  import { entryKindPresentation } from '../../explore/labels';
  import type { ExploreFilter } from '../../explore/models';
  import AccountFilter from './AccountFilter.svelte';
  import IdentityFilter from './IdentityFilter.svelte';
  import { popoverStyle } from './popover-position';

  interface KindOption {
    name: string;
    value: string;
  }

  let {
    client,
    filters,
    extraActive = false,
    lockedFilter = undefined,
    onChange,
    extra = undefined,
    onClear = undefined,
  }: {
    extra?: Snippet;
    extraActive?: boolean;
    onClear?: () => void;
    client: APIClient;
    filters: ExploreFilter[];
    lockedFilter?: ExploreFilter;
    onChange: (filters: ExploreFilter[]) => void;
  } = $props();

  const api = createExploreAPI(untrack(() => client));
  const visualStatusId = $props.id();
  const filterCount = $derived(filters.length + Number(extraActive));
  let open = $state(false);
  let triggerElement = $state<HTMLSpanElement>();
  let panelElement = $state<HTMLDivElement>();
  let panelStyle = $state('');
  let kinds = $state<KindOption[]>([]);
  let kindsStatus = $state('');
  let controller: AbortController | undefined;

  const kindValues = $derived(filters.find((filter) => filter.dimension === 'kind')?.values ?? []);
  let dateError = $state('');

  async function loadKinds(): Promise<void> {
    controller?.abort();
    kindsStatus = 'Loading kinds…';
    controller = new AbortController();
    try {
      const loaded = await api.groups({ limit: 50 }, 'kind', controller.signal);
      if (loaded.status !== 'ready') {
        kindsStatus = loaded.status === 'unavailable' ? 'Kinds unavailable' : 'Could not load kinds';
        return;
      }
      kinds = loaded.result.rows.map((row) => ({ name: entryKindPresentation(row.key).name, value: row.key }));
      kindsStatus = kinds.length ? '' : 'No message kinds';
    } catch {
      kindsStatus = 'Could not load kinds';
    }
  }

  function position(): void {
    if (!triggerElement || !panelElement) return;
    panelStyle = popoverStyle(triggerElement, panelElement);
  }

  async function toggle(): Promise<void> {
    open = !open;
    if (!open) return;
    void loadKinds();
    await tick();
    position();
  }

  $effect(() => {
    if (!open) return;
    return autoReposition(() => [panelElement, triggerElement], position);
  });

  onDestroy(() => controller?.abort());

  function kindChecked(option: KindOption): boolean {
    return kindValues.includes(option.value);
  }

  function toggleKind(option: KindOption, checked: boolean): void {
    const values = checked ? [...new Set([...kindValues, option.value])] : kindValues.filter((value) => value !== option.value);
    const rest = filters.filter((filter) => filter.dimension !== 'kind');
    onChange(values.length > 0 ? [...rest, { dimension: 'kind', values }] : rest);
  }

  function dateValue(dimension: 'after' | 'before'): string {
    const value = filters.find((filter) => filter.dimension === dimension)?.values[0];
    if (!value) return '';
    return boundaryDate(value, dimension === 'before');
  }

  function changeDate(dimension: 'after' | 'before', input: HTMLInputElement): void {
    const value = input.value;
    dateError = '';
    const from = dimension === 'after' ? value : dateValue('after');
    const to = dimension === 'before' ? value : dateValue('before');
    if (from && to && from > to) {
      dateError = 'From must be on or before To.';
      input.value = dateValue(dimension);
      return;
    }
    const rest = filters.filter((filter) => filter.dimension !== dimension);
    if (!value) return onChange(rest);
    const current = filters.find((filter) => filter.dimension === dimension)?.values[0];
    const peer = filters.find((filter) => filter.dimension === (dimension === 'after' ? 'before' : 'after'))?.values[0];
    onChange([...rest, { dimension, values: [editedBoundary(value, dimension === 'before', isLocalBound(current ?? peer))] }]);
  }

  function clear(): void {
    dateError = '';
    if (onClear) onClear();
    else onChange([]);
  }
</script>

<svelte:document onmousedown={(event) => {
  if (open && ![panelElement, triggerElement].some((element) => element?.contains(event.target as Node))) open = false;
}} onkeydown={(event) => {
  // kit-ui-check-ignore: observes the nested Kit picker's open state.
  if (!open || document.querySelector('[aria-modal="true"]') || panelElement?.querySelector('[role="combobox"][aria-expanded="true"]')) return;
  escapeCloses(() => { open = false; triggerElement?.querySelector('button')?.focus(); })(event);
}} />

<span class="filter-trigger" bind:this={triggerElement}>
  <Button
    surface={open || filterCount > 0 ? 'soft' : 'outline'}
    label={filterCount > 0 ? `Filter · ${filterCount}` : 'Filter'}
    ariaLabel="Filter"
    ariaDescribedby={extraActive ? visualStatusId : undefined}
    ariaExpanded={open}
    onclick={() => void toggle()}
  />
</span>
{#if extraActive}<span id={visualStatusId} class="kit-sr-only">Visual search active</span>{/if}

{#if open}
  <div class="filter-panel kit-popover-card" role="dialog" aria-label="Filter" data-form bind:this={panelElement} style={panelStyle}>
    {#if kinds.length > 0}
      <fieldset class="field">
        <legend>Kind</legend>
        <div class="kinds">
          {#each kinds as option (option.name)}
            <Checkbox
              label={option.name}
              checked={kindChecked(option)}
              onchange={(checked) => toggleKind(option, checked)}
            />
          {/each}
        </div>
      </fieldset>
    {/if}
    {#if kindsStatus}<p role="status">{kindsStatus}</p>{/if}
    <AccountFilter {client} {filters} {onChange} showSingleMailbox />
    {#if !lockedFilter}<IdentityFilter {client} {filters} {onChange} />{/if}
    <fieldset class="field">
      <legend>{filters.some((filter) => (filter.dimension === 'after' || filter.dimension === 'before') && isLocalBound(filter.values[0])) ? 'Dates' : 'Dates (UTC)'}</legend>
      <div class="dates">
      <!-- kit-ui-check-ignore: independent optional boundaries; DateRangePicker commits complete ranges. -->
      <label>From<input type="date" aria-label={isLocalBound(filters.find((filter) => filter.dimension === 'before')?.values[0]) && !isLocalBound(filters.find((filter) => filter.dimension === 'after')?.values[0]) && filters.some((filter) => filter.dimension === 'after') ? 'From (UTC)' : 'From'} value={dateValue('after')} onchange={(event) => changeDate('after', event.currentTarget)} /></label>
      <!-- kit-ui-check-ignore: independent optional boundaries; DateRangePicker commits complete ranges. -->
      <label>To<input type="date" aria-label={isLocalBound(filters.find((filter) => filter.dimension === 'after')?.values[0]) && !isLocalBound(filters.find((filter) => filter.dimension === 'before')?.values[0]) && filters.some((filter) => filter.dimension === 'before') ? 'To (UTC)' : 'To'} value={dateValue('before')} onchange={(event) => changeDate('before', event.currentTarget)} /></label>
      </div>
    </fieldset>
    {#if dateError}<p role="alert">{dateError}</p>{/if}
    {@render extra?.()}
    {#if filters.length > 0 || extra}
      <div class="panel-footer">
        <Button surface="soft" label="Clear filters" onclick={clear} />
      </div>
    {/if}
  </div>
{/if}

<style>
  /* A Meta label 4 above each field, the two dates a pair 12 apart. */
  .dates { display: grid; grid-template-columns: 1fr 1fr; gap: var(--space-box); }
  .dates label { display: grid; gap: var(--space-tight); color: var(--text-muted); font-size: var(--type-meta-size); line-height: var(--type-meta-line); }
  .dates input { min-width: 0; width: 100%; height: var(--field-h); box-sizing: border-box; padding: 0 var(--field-pad-x); color: var(--text-primary); background: var(--bg-canvas); border: 1px solid var(--border-default); border-radius: var(--radius-sm); font: inherit; font-size: var(--field-text-size); line-height: var(--type-body-line); }
  .filter-trigger {
    display: inline-flex;
  }

  .filter-panel {
    position: fixed;
    z-index: var(--z-popover);
    display: grid;
    width: 340px;
    max-width: calc(100vw - 2 * var(--space-4));
    max-height: min(520px, calc(100vh - 2 * var(--space-4)));
    --kit-control-height: var(--control-h);
    gap: var(--section-gap);
    padding: var(--pane-pad-x);
    overflow-y: auto;
    overscroll-behavior: contain;
  }

  .field {
    display: grid;
    min-width: 0;
    gap: var(--label-gap);
    margin: 0;
    padding: 0;
    border: 0;
  }

  legend {
    padding: 0;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  .kinds {
    display: flex;
    flex-wrap: wrap;
    gap: var(--control-gap) var(--section-gap);
  }

  .panel-footer {
    display: flex;
    justify-content: flex-end;
  }
</style>
