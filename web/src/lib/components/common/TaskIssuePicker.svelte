<script lang="ts">
  import { debounce, RadioGroup, SearchInput } from '@kenn-io/kit-ui';
  import { onMount } from 'svelte';

  let { load, noun, empty, disabled = false, moreNote = '', hint = '', showCount = false, searchLabel = `Filter ${noun}`, selected = $bindable('') }: {
    load: (query: string, signal: AbortSignal) => Promise<{ items: { id: string; label: string; detail?: string }[]; truncated: boolean }>;
    moreNote?: string;
    hint?: string;
    showCount?: boolean;
    searchLabel?: string;
    noun: string;
    empty: string;
    disabled?: boolean;
    selected?: string;
  } = $props();
  let query = $state('');
  let listedQuery = $state('');
  let items = $state<{ id: string; label: string; detail?: string }[]>([]);
  let truncated = $state(false);
  let loading = $state(true);
  let error = $state('');
  let showMore = $derived(!!moreNote && !error && truncated && !loading && query.trim() === listedQuery);
  let controller: AbortController | undefined;
  async function reload() {
    controller?.abort();
    const current = controller = new AbortController();
    selected = ''; loading = true; error = ''; listedQuery = query.trim();
    try {
      const result = await load(listedQuery, current.signal);
      if (!current.signal.aborted) { items = result.items; truncated = result.truncated; }
    } catch (cause) {
      if (!current.signal.aborted) { items = []; error = cause instanceof Error ? cause.message : `Unable to load ${noun}.`; }
    } finally {
      if (!current.signal.aborted) loading = false;
    }
  }
  const schedule = debounce(() => void reload(), 250);
  onMount(() => {
    void reload();
    return () => { schedule.cancel(); controller?.abort(); };
  });
</script>

<div class="picker">
  <SearchInput block value={query} ariaLabel={searchLabel} placeholder={searchLabel} {disabled} oninput={(value) => { selected = ''; query = value; schedule(); }} />
  <div class="results">
    {#if loading || query.trim() !== listedQuery}<p role="status">Loading…</p>
    {:else}
      {#if error}<p role="alert">{error}</p>{/if}
      {#if items.length}{#if showCount}<p>{items.length} {items.length === 1 ? 'result' : 'results'}</p>{/if}<RadioGroup legend={noun[0].toUpperCase() + noun.slice(1)} options={items.map(item => ({ value: item.id, label: item.detail ? `${item.label} · ${item.detail}` : item.label }))} value={selected} {disabled} onchange={(value) => selected = value ?? ''} />
      {:else if !error}<p>{listedQuery ? `No ${noun} match "${listedQuery}".` : empty}</p>{/if}
    {/if}
  </div>
  {#if hint || showMore}<p>{hint}{hint && showMore ? ' ' : ''}{showMore ? moreNote : ''}</p>{/if}
</div>

<style>
  .picker { display: grid; gap: var(--space-2); min-width: 0; }
  .picker :global(.kit-text-input__control) { font-size: 16px; }
  .results { height: 13.875rem; box-sizing: border-box; overflow: auto; border: 1px solid var(--border-muted); border-radius: var(--radius-sm); padding: var(--space-3) 12px; }
  /* kit-ui-check-ignore: RadioGroup does not expose a legend class. */
  .results :global(.kit-radio-group__legend) { position: absolute; width: 1px; height: 1px; padding: 0; margin: -1px; overflow: hidden; clip-path: inset(50%); white-space: nowrap; border: 0; }
  p { margin: 0; color: var(--text-secondary); font-size: 12px; }
</style>
