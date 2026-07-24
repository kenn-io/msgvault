<script lang="ts">
  import { SearchInput } from '@kenn-io/kit-ui';
  import { untrack } from 'svelte';

  import type { ArchiveSearchMode } from '../../archive/types';
  import SearchModeControl from './SearchModeControl.svelte';

  interface Props {
    initialQuery: string;
    initialMode: ArchiveSearchMode;
    onSubmit: (query: string, mode: ArchiveSearchMode) => void;
  }

  let { initialQuery, initialMode, onSubmit }: Props = $props();

  let query = $state(untrack(() => initialQuery));
  let mode = $state<ArchiveSearchMode>(untrack(() => initialMode));

  $effect(() => {
    query = initialQuery;
    mode = initialMode;
  });

  function handleSubmit(event: SubmitEvent): void {
    event.preventDefault();
    onSubmit(query.trim(), mode);
  }
</script>

<form role="search" aria-label="Search archive" onsubmit={handleSubmit}>
  <div class="query">
    <SearchInput
      id="archive-search-input"
      name="q"
      bind:value={query}
      block
      placeholder="Search the archive…"
      ariaLabel="Search the archive"
    />
  </div>
  <SearchModeControl
    requestedMode={mode === 'fts' ? 'full_text' : mode === 'vector' ? 'semantic' : 'hybrid'}
    onchange={(next) => {
      mode = next === 'full_text' ? 'fts' : next === 'semantic' ? 'vector' : 'hybrid';
    }}
  />
  <button type="submit">Search</button>
</form>

<style>
  form {
    display: flex;
    align-items: center;
    gap: 8px;
    width: 100%;
  }

  .query {
    min-width: 0;
    flex: 1;
  }

  button[type='submit'] {
    padding: 5px 12px;
    border: 1px solid var(--border-default);
    border-radius: 4px;
    background: var(--bg-surface);
    color: var(--text-primary);
    cursor: pointer;
  }
</style>
