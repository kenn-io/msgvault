<script lang="ts">
  import { debounce, RadioGroup, SearchInput } from '@kenn-io/kit-ui';
  import { onMount } from 'svelte';

  let { load, noun, empty, disabled = false, moreNote = '', selected = $bindable('') }: {
    load: (query: string, signal: AbortSignal) => Promise<{ items: { id: string; label: string; detail?: string }[]; truncated: boolean }>;
    moreNote?: string;
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
  let controller: AbortController | undefined;
  async function reload() {
    controller?.abort();
    const current = controller = new AbortController();
    selected = ''; loading = true; error = ''; listedQuery = query.trim();
    try {
      const result = await load(listedQuery, current.signal);
      if (!current.signal.aborted) { items = result.items; truncated = result.truncated; }
    } catch (cause) {
      if (!current.signal.aborted) error = cause instanceof Error ? cause.message : `Unable to load ${noun}.`;
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
  <SearchInput block value={query} ariaLabel={`Filter ${noun}`} placeholder={`Filter ${noun}`} {disabled} oninput={(value) => { selected = ''; query = value; schedule(); }} />
  {#if loading || query.trim() !== listedQuery}<p role="status">Loading…</p>
  {:else if error}<p role="alert">{error}</p>
  {:else if items.length === 0}<p>{listedQuery ? `No ${noun} match "${listedQuery}".` : empty}</p>
  {:else}
    <div class="results" class:overflowing={items.length > 5}>
      <RadioGroup legend={noun[0].toUpperCase() + noun.slice(1)} options={items.map(item => ({ value: item.id, label: item.detail ? `${item.label} · ${item.detail}` : item.label }))} value={selected} {disabled} onchange={(value) => selected = value ?? ''} />
    </div>
  {/if}
  {#if !loading && query.trim() === listedQuery && !error && truncated}<p class="more-note">{moreNote}</p>{/if}
</div>

<style>
  .picker { display: grid; gap: var(--space-2); min-width: 0; }
  .picker :global(.kit-text-input__control) { font-size: 16px; }
  .picker :global(.kit-text-input:has(.kit-text-input__control:focus-visible)) { outline-width: 1px; border-color: var(--border-default); }
  :global(:root.high-contrast) .picker :global(.kit-text-input:has(.kit-text-input__control:focus-visible)) { outline-width: 3px; }
  .results { max-height: 16rem; overflow: auto; border: 1px solid var(--border-muted); border-radius: var(--radius-sm); padding: 12px; }
  .results.overflowing { mask-image: linear-gradient(to bottom, black calc(100% - 16px), transparent); }
  .results :global(.kit-radio-group__legend) { font-weight: 400; }
  .results :global(.kit-radio-group__option) { min-height: 32px; }
  p { margin: 0; color: var(--text-secondary); font-size: 12px; }
  .more-note { margin-top: var(--space-2); }
</style>
