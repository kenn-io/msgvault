<script lang="ts">
  import { Button, debounce, Typeahead, type TypeaheadOption } from '@kenn-io/kit-ui';
  import { onDestroy, untrack } from 'svelte';

  import type { APIClient } from '../../api/client';
  import {
    getPersonProfile as generatedGetPersonProfile,
    listDirectoryPeople as generatedListDirectoryPeople
  } from '../../api/generated/api/api';
  import type { FactLedgerController } from '../../directory/fact-ledger-controller.svelte';
  import type { DirectoryPerson } from '../../directory/models';
  import FactLedger from './FactLedger.svelte';

  const SEARCH_LIMIT = 20;
  const SEARCH_DEBOUNCE_MS = 250;

  interface Props {
    controller: FactLedgerController;
    client: APIClient;
    personID: number | null;
    onSelectFactPerson: (personID: number) => void;
    onOpenPerson?: (personID: number) => void;
  }
  let { controller, client, personID, onSelectFactPerson, onOpenPerson = () => undefined }: Props = $props();

  let people = $state<DirectoryPerson[]>([]);
  let searching = $state(false);
  let searchError = $state('');
  let named = $state<{ id: number; name: string } | null>(null);
  let nameLoadFailed = $state(false);
  let searchAbort: AbortController | undefined;
  let searchGeneration = 0;

  const personName = $derived(named !== null && named.id === personID ? named.name : null);
  const peopleOptions = $derived(
    people.map((person): TypeaheadOption => ({ name: String(person.id), label: personLabel(person) }))
  );
  const debouncedSearch = debounce((value: string) => void searchPeople(value), SEARCH_DEBOUNCE_MS);

  $effect(() => {
    const id = personID;
    if (id === null || untrack(() => personName) !== null) return;
    const abort = new AbortController();
    nameLoadFailed = false;
    void loadPersonName(id, abort.signal);
    return () => abort.abort();
  });
  onDestroy(() => {
    debouncedSearch.cancel();
    searchAbort?.abort();
  });

  function personLabel(person: DirectoryPerson): string {
    return person.display_name ?? `Person ${person.id}`;
  }
  async function loadPersonName(id: number, signal: AbortSignal): Promise<void> {
    try {
      const response = await generatedGetPersonProfile({ id }, { ...client, signal });
      if (signal.aborted) return;
      if (!response.data) {
        nameLoadFailed = true;
        return;
      }
      if (response.data.display_name) named = { id, name: response.data.display_name };
    } catch {
      if (!signal.aborted) nameLoadFailed = true;
    }
  }
  async function searchPeople(value: string): Promise<void> {
    const query = value.trim();
    searchAbort?.abort();
    if (!query) {
      people = [];
      searching = false;
      searchError = '';
      return;
    }
    const abort = new AbortController();
    searchAbort = abort;
    const generation = ++searchGeneration;
    searching = true;
    searchError = '';
    try {
      const response = await generatedListDirectoryPeople(
        { q: query, limit: SEARCH_LIMIT },
        { ...client, signal: abort.signal }
      );
      if (abort.signal.aborted || generation !== searchGeneration) return;
      if (response.data) {
        people = response.data.people ?? [];
      } else {
        searchError = failureMessage(response.error, response.response.status);
      }
    } catch (cause: unknown) {
      if (!abort.signal.aborted && generation === searchGeneration) searchError = failureMessage(cause, 0);
    } finally {
      if (generation === searchGeneration) searching = false;
    }
  }
  function selectPerson(value: string): void {
    const id = Number(value);
    const chosen = people.find((person) => person.id === id);
    if (chosen) named = { id, name: personLabel(chosen) };
    onSelectFactPerson(id);
  }
  function failureMessage(value: unknown, status: number): string {
    if (typeof value === 'object' && value !== null && 'message' in value && typeof value.message === 'string')
      return value.message;
    if (value instanceof Error && value.message) return value.message;
    return status ? `Request failed (${status}).` : 'Request failed.';
  }
</script>

<section class="fact-review" aria-labelledby="fact-review-heading" data-review-section>
  <h2 id="fact-review-heading" class="kit-sr-only review-heading" tabindex="-1">Facts</h2>
  <label class="person-field">
    <span class="field-label">Person</span>
    <Typeahead
      title="Fact person"
      placeholder="Person"
      options={peopleOptions}
      value={personID === null ? '' : String(personID)}
      fallbackLabel={personName ?? (personID === null ? 'Choose a person' : `Person ${personID}`)}
      remote
      loading={searching}
      loadingLabel="Searching…"
      emptyLabel="No matching people"
      error={searchError}
      onquery={debouncedSearch}
      onselect={selectPerson}
    />
  </label>
  {#if personID === null}
    <p class="choose-hint">Choose a person to see the facts recorded about them.</p>
  {:else}
    <div class="person-context">
      <strong>{personName ?? `Person ${personID}`}</strong>
      <Button label="Open person profile" size="sm" onclick={() => onOpenPerson(personID)} />
    </div>
    {#if nameLoadFailed}
      <p class="name-note">Couldn't load this person's name.</p>
    {/if}
    <div class="notices" aria-label="Unavailable fact features">
      <p>Fact candidate decisions are unavailable until a generated candidate contract is installed.</p>
      <p>A dated last-time-we-talked brief is unavailable until the server exposes a generated brief contract.</p>
    </div>
    <FactLedger {controller} />
  {/if}
</section>

<style>
  .fact-review { display: grid; gap: var(--space-4); }
  h2 { margin: 0; }
  [data-review-section] { position: relative; border-radius: var(--radius-md); }
  [data-review-section]:has(> .review-heading:focus-visible) {
    outline: 2px solid var(--focus-color); outline-offset: 4px;
  }
  .person-field { display: grid; gap: var(--space-1); justify-items: start; }
  .field-label { color: var(--text-secondary); font-size: var(--font-size-sm); }
  .name-note { margin: 0; color: var(--text-muted); font-size: var(--font-size-sm); }
  .choose-hint { margin: 0; color: var(--text-secondary); }
  .person-context { display: flex; align-items: center; justify-content: space-between; gap: var(--space-3); flex-wrap: wrap; }
  .notices { display: grid; gap: var(--space-2); padding: var(--space-3); border-left: 2px solid var(--border-strong); color: var(--text-secondary); }
  .notices p { margin: 0; }
</style>
