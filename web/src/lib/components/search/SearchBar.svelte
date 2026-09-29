<script lang="ts">
  import { Button, SearchInput, SelectDropdown } from '@kenn-io/kit-ui';
  import { untrack } from 'svelte';

  import type { ExploreSearchMode, ExploreWorkspace } from '../../explore/models';
  import SearchModeControl from './SearchModeControl.svelte';

  interface Props {
    workspace: ExploreWorkspace;
    query: string;
    mode: ExploreSearchMode;
    live: boolean;
    compact: boolean;
    onDraft: (query: string, mode: ExploreSearchMode) => void;
    onSubmit: (query: string, mode: ExploreSearchMode) => void;
    inputEl?: HTMLInputElement;
  }

  let { workspace, query, mode, live, compact, onDraft, onSubmit, inputEl = $bindable() }: Props = $props();

  let draft = $state(untrack(() => query));
  let draftMode = $state<ExploreSearchMode>(untrack(() => mode));

  // Deriveds re-run the effects below only when a value actually changes, so
  // a workspace change discards an unsubmitted draft while a mode change alone
  // keeps the typed query.
  const committedWorkspace = $derived(workspace);
  const committedQuery = $derived(query);
  const committedMode = $derived(mode);

  $effect(() => {
    void committedWorkspace;
    draft = committedQuery;
  });
  $effect(() => {
    void committedWorkspace;
    draftMode = committedMode;
  });

  const modeOptions = [
    { value: 'full_text', label: 'Full text' },
    { value: 'semantic', label: 'Semantic' },
    { value: 'hybrid', label: 'Hybrid' }
  ];

  function changeQuery(value: string): void {
    draft = value;
    if (live) onDraft(value, draftMode);
  }

  function changeMode(value: ExploreSearchMode): void {
    draftMode = value;
    if (live) onDraft(draft, value);
  }

  function submit(event: SubmitEvent): void {
    event.preventDefault();
    onSubmit(draft.trim(), draftMode);
  }
</script>

<form
  class="global-search"
  class:global-search--compact={compact}
  role="search"
  aria-label="Search Everything"
  onsubmit={submit}
>
  <div class="global-search__query">
    <SearchInput
      id="everything-search"
      bind:inputEl
      value={draft}
      ariaLabel="Search everything"
      placeholder="Search people, conversations, events, and files…"
      block
      oninput={changeQuery}
    />
  </div>
  {#if compact}
    <SelectDropdown
      title="Search mode"
      value={draftMode}
      options={modeOptions}
      align="end"
      onchange={(value) => changeMode(value as ExploreSearchMode)}
    />
  {:else}
    <SearchModeControl requestedMode={draftMode} onchange={changeMode} />
    <Button type="submit" label="Search" surface="soft" />
  {/if}
</form>

<style>
  .global-search {
    display: flex;
    min-width: 0;
    flex: 1;
    align-items: center;
    gap: var(--space-3);
  }

  .global-search__query {
    min-width: 0;
    flex: 1;
    max-width: 640px;
  }

  .global-search--compact {
    gap: var(--space-2);
  }
</style>
