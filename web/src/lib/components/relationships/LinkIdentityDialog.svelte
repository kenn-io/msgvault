<script lang="ts">
  import {
    appShortcuts,
    Button,
    debounce,
    Modal,
    Typeahead,
    type TypeaheadOption
  } from '@kenn-io/kit-ui';
  import { onDestroy, onMount } from 'svelte';

  import type { APIClient } from '../../api/client';
  import type { PersonSummary } from '../../explore/models';
  import type { LinkOutcome } from '../../relationships/controller.svelte';
  import type { ValidatedPersonMergeRequired } from '../../directory/person-merge';

  const SEARCH_DEBOUNCE_MS = 250;
  const SEARCH_LIMIT = 20;

  interface Props {
    client: APIClient;
    /** The currently open cluster's own ID, excluded from results — linking
     * it to itself is rejected by the API and never a meaningful choice. */
    excludeID: number;
    /** The open person's display name, woven into the dialog title so the
     * flow reads in human terms ("Link another identity for Alice"). */
    personLabel: string;
    onConfirm: (participantID: number) => Promise<LinkOutcome>;
    onMergeRequired?: (conflict: ValidatedPersonMergeRequired) => void;
    onClose: () => void;
  }

  let { client, excludeID, personLabel, onConfirm, onMergeRequired = undefined, onClose }: Props = $props();

  let query = $state('');
  let results = $state<PersonSummary[]>([]);
  let searching = $state(false);
  let searchError = $state<string | null>(null);
  let selectedID = $state<number | null>(null);
  let confirming = $state(false);
  let confirmError = $state<string | null>(null);
  let preserveSelectionOnClose = false;
  const options = $derived(results.map((row): TypeaheadOption => ({
    name: String(row.id),
    label: row.display_label,
    meta: identifiersSummary(row)
  })));

  let searchAbort: AbortController | undefined;
  let searchGeneration = 0;
  let releaseShortcutScope: (() => void) | undefined;

  const debouncedSearch = debounce((value: string) => void runSearch(value), SEARCH_DEBOUNCE_MS);

  // Suspends the app's global shortcuts (command palette, grid navigation,
  // etc.) while the dialog is open, matching every other Modal consumer
  // (FileViewer, DeletionsWorkspace) instead of leaving it the one that
  // doesn't push a scope.
  onMount(() => {
    releaseShortcutScope = appShortcuts.pushScope('link-identity-dialog');
  });

  // Without this, closing the dialog mid-debounce (or mid-request) leaves
  // the pending timer and in-flight fetch running: the timer fires after
  // unmount and issues a search the user never sees, and the request
  // itself keeps a connection open for no reason. The generation counter in
  // runSearch already guards against a late response updating state, but it
  // doesn't stop the request from being sent at all.
  onDestroy(() => {
    debouncedSearch.cancel();
    searchAbort?.abort();
    releaseShortcutScope?.();
  });

  async function runSearch(value: string): Promise<void> {
    const trimmed = value.trim();
    searchAbort?.abort();
    if (trimmed === '') {
      results = [];
      searching = false;
      searchError = null;
      return;
    }
    const controller = new AbortController();
    searchAbort = controller;
    const generation = ++searchGeneration;
    searching = true;
    searchError = null;
    try {
      const { data, error, response } = await client.POST('/api/v1/participants/search', {
        body: {
          predicate: {},
          identity_query: trimmed,
          sort: { field: 'activity_count', direction: 'desc' },
          limit: SEARCH_LIMIT
        },
        signal: controller.signal
      });
      if (generation !== searchGeneration || controller.signal.aborted) return;
      if (data) {
        results = (data.rows ?? []).filter((row) => row.id !== excludeID);
        return;
      }
      searchError = messageFor(error, response.status);
    } catch (cause: unknown) {
      if (generation === searchGeneration && !controller.signal.aborted) searchError = messageFor(cause, 0);
    } finally {
      if (generation === searchGeneration) searching = false;
    }
  }

  function handleQueryInput(value: string): void {
    query = value;
    if (value.trim() === '' && preserveSelectionOnClose) {
      preserveSelectionOnClose = false;
    } else {
      selectedID = null;
      confirmError = null;
    }
    debouncedSearch(value);
  }

  function selectResult(id: number): void {
    selectedID = id;
    confirmError = null;
    // Typeahead reports an empty query as it closes after selection. Preserve
    // this result through that lifecycle reset; a later empty editable field
    // clears it before confirmation can use stale state.
    preserveSelectionOnClose = true;
  }

  async function confirmLink(): Promise<void> {
    if (selectedID === null || confirming) return;
    confirming = true;
    confirmError = null;
    try {
      const outcome = await onConfirm(selectedID);
      if (outcome.ok) {
        onClose();
        return;
      }
      if (outcome.code === 'merge_required') {
        if (onMergeRequired) onMergeRequired(outcome.conflict);
        else confirmError = outcome.message;
        return;
      }
      confirmError = outcome.code === 'already_linked'
        ? 'Already linked — these two are treated as the same person.'
        : outcome.message;
    } finally {
      confirming = false;
    }
  }

  /** Every dismissal path (Cancel, Escape, backdrop, the × button) funnels
   * through here: while a confirm is in flight the dialog must stay visible
   * until the outcome is known — hiding it would let the link land (or fail)
   * invisibly. Same idiom as CreateTaskDialog. */
  function requestClose(): void {
    if (confirming) return;
    onClose();
  }

  function identifiersSummary(row: PersonSummary): string {
    const labels = (row.identifiers ?? []).map((identifier) => identifier.display_value?.trim() || identifier.value);
    return labels.length > 0 ? labels.join(', ') : 'No stored identifiers';
  }

  function messageFor(value: unknown, status: number): string {
    if (typeof value === 'object' && value !== null && 'message' in value) {
      const message = (value as { message?: unknown }).message;
      if (typeof message === 'string' && message) return message;
    }
    return status ? `Search failed (${status})` : 'Search failed';
  }
</script>

<Modal
  title={`Link another identity for ${personLabel}`}
  ariaLabel={`Link another identity for ${personLabel}`}
  onclose={requestClose}
>
  <div class="link-identity-dialog" aria-busy={confirming}>
    <Typeahead
      options={options}
      value={selectedID === null ? '' : String(selectedID)}
      fallbackLabel="Choose a person"
      placeholder="Search people to link"
      title="Person to link"
      emptyLabel="No matching people found"
      loading={searching}
      loadingLabel="Searching…"
      remote
      onquery={handleQueryInput}
      error={searchError ?? ''}
      onselect={(value) => {
        selectResult(Number(value));
      }}
    />
    {#if confirmError}
      <p class="confirm-error" role="alert">{confirmError}</p>
    {/if}
  </div>
  {#snippet footer()}
    <Button surface="soft" label="Cancel" disabled={confirming} onclick={requestClose} />
    <Button
      tone="info"
      surface="solid"
      label="These are the same person"
      disabled={selectedID === null || confirming}
      onclick={() => void confirmLink()}
    />
  {/snippet}
</Modal>

<style>
  .link-identity-dialog {
    display: flex;
    min-width: 20rem;
    flex-direction: column;
    gap: var(--space-3);
  }

  .confirm-error {
    margin: 0;
    color: var(--text-danger);
    font-size: var(--font-size-xs);
  }
</style>
