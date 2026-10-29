<script lang="ts">
  import { autoReposition, Button, Checkbox, escapeCloses, floatingPopoverStyle } from '@kenn-io/kit-ui';
  import { onDestroy, tick, untrack } from 'svelte';

  import type { APIClient } from '../../api/client';
  import { createExploreAPI } from '../../explore/api';
  import { entryKindPresentation } from '../../explore/labels';
  import type { ExploreFilter } from '../../explore/models';
  import AccountFilter from './AccountFilter.svelte';
  import IdentityFilter from './IdentityFilter.svelte';

  interface KindOption {
    name: string;
    values: string[];
  }

  let {
    client,
    filters,
    onChange,
  }: {
    client: APIClient;
    filters: ExploreFilter[];
    onChange: (filters: ExploreFilter[]) => void;
  } = $props();

  const api = createExploreAPI(untrack(() => client));
  let open = $state(false);
  let triggerElement = $state<HTMLSpanElement>();
  let panelElement = $state<HTMLDivElement>();
  let panelStyle = $state('');
  let kinds = $state<KindOption[]>([]);
  let kindsStatus = $state('');
  let kindsLoaded = false;
  let controller: AbortController | undefined;

  const kindValues = $derived(filters.find((filter) => filter.dimension === 'message_type')?.values ?? []);
  let dateError = $state('');

  async function loadKinds(): Promise<void> {
    if (kindsLoaded) return;
    kindsLoaded = true;
    kindsStatus = 'Loading kinds…';
    controller = new AbortController();
    try {
      const loaded = await api.groups({ limit: 50 }, 'message_type', controller.signal);
      if (loaded.status !== 'ready') {
        kindsStatus = loaded.status === 'unavailable' ? 'Kinds unavailable' : 'Could not load kinds';
        kindsLoaded = false;
        return;
      }
      // Several message types read as one kind, such as every chat network.
      const byName = new Map<string, string[]>();
      for (const row of loaded.result.rows) {
        const name = entryKindPresentation('', row.key).name;
        byName.set(name, [...(byName.get(name) ?? []), row.key]);
      }
      kinds = [...byName].map(([name, values]) => ({ name, values }));
      kindsStatus = kinds.length ? '' : 'No message kinds';
    } catch {
      kindsLoaded = false;
      kindsStatus = 'Could not load kinds';
    }
  }

  function position(): void {
    if (!triggerElement || !panelElement) return;
    panelStyle = floatingPopoverStyle({
      trigger: triggerElement.getBoundingClientRect(),
      viewportWidth: window.innerWidth,
      viewportHeight: window.innerHeight,
      popoverWidth: panelElement.offsetWidth,
      popoverHeight: panelElement.offsetHeight,
      align: 'start',
    });
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
    return option.values.some((value) => kindValues.includes(value));
  }

  function toggleKind(option: KindOption, checked: boolean): void {
    const values = checked ? [...new Set([...kindValues, ...option.values])] : kindValues.filter((value) => !option.values.includes(value));
    const rest = filters.filter((filter) => filter.dimension !== 'message_type');
    onChange(values.length > 0 ? [...rest, { dimension: 'message_type', values }] : rest);
  }

  function dateValue(dimension: 'after' | 'before'): string {
    const value = filters.find((filter) => filter.dimension === dimension)?.values[0];
    if (!value) return '';
    const date = new Date(value);
    if (dimension === 'before') date.setUTCMilliseconds(date.getUTCMilliseconds() - 1);
    return `${date.getUTCFullYear()}-${String(date.getUTCMonth() + 1).padStart(2, '0')}-${String(date.getUTCDate()).padStart(2, '0')}`;
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
    const boundary = new Date(`${value}T00:00:00Z`).getTime() + (dimension === 'before' ? 86400000 : 0);
    onChange([...rest, { dimension, values: [new Date(boundary).toISOString()] }]);
  }

  function clear(): void {
    dateError = '';
    onChange([]);
  }
</script>

<svelte:document onmousedown={(event) => {
  if (open && ![panelElement, triggerElement].some((element) => element?.contains(event.target as Node))) open = false;
}} onkeydown={(event) => {
  // kit-ui-check-ignore: observes the nested Kit picker's open state.
  if (!open || panelElement?.querySelector('[role="combobox"][aria-expanded="true"]')) return;
  escapeCloses(() => { open = false; triggerElement?.querySelector('button')?.focus(); })(event);
}} />

<span class="filter-trigger" bind:this={triggerElement}>
  <Button
    surface={open || filters.length > 0 ? 'soft' : 'outline'}
    label={filters.length > 0 ? `Filter · ${filters.length}` : 'Filter'}
    ariaLabel="Filter"
    ariaExpanded={open}
    onclick={() => void toggle()}
  />
</span>

{#if open}
  <div class="filter-panel kit-popover-card" role="dialog" aria-label="Filter" bind:this={panelElement} style={panelStyle}>
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
    <IdentityFilter {client} {filters} {onChange} />
    <fieldset class="field">
      <legend>Dates (UTC)</legend>
      <div class="dates">
      <!-- kit-ui-check-ignore: independent optional boundaries; DateRangePicker commits complete ranges. -->
      <label>From<input type="date" aria-label="From" value={dateValue('after')} onchange={(event) => changeDate('after', event.currentTarget)} /></label>
      <!-- kit-ui-check-ignore: independent optional boundaries; DateRangePicker commits complete ranges. -->
      <label>To<input type="date" aria-label="To" value={dateValue('before')} onchange={(event) => changeDate('before', event.currentTarget)} /></label>
      </div>
    </fieldset>
    {#if dateError}<p role="alert">{dateError}</p>{/if}
    {#if filters.length > 0}
      <div class="panel-footer">
        <Button surface="soft" label="Clear filters" onclick={clear} />
      </div>
    {/if}
  </div>
{/if}

<style>
  .dates { display: grid; grid-template-columns: 1fr 1fr; gap: var(--space-3); }
  .dates label { display: grid; gap: var(--space-1); color: var(--text-muted); font-size: var(--font-size-xs); }
  .dates input { min-width: 0; width: 100%; box-sizing: border-box; padding: var(--space-2); color: var(--text-primary); background: var(--bg-canvas); border: 1px solid var(--border-default); border-radius: var(--radius-sm); font: inherit; }
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
