<script lang="ts">
  import { Button, Chip, IconButton, TextInput, Typeahead } from '@kenn-io/kit-ui';
  import X from '@lucide/svelte/icons/x';
  import { onDestroy, untrack } from 'svelte';
  import DetailRow from '../people/DetailRow.svelte';
  import {
    createPersonAgendaItem,
    getKataIntegrationStatus,
    listPersonAgenda,
    unlinkPersonAgendaItem,
  } from '../../api/generated/api/api';
  import type { PersonAgendaItem as AgendaItem } from '../../api/generated/models';
  import type { APIClient } from '../../api/client';

  interface Props {
    client: APIClient;
    personID: number;
    refreshKey?: string;
    onSetUp?: () => void;
    onAnnounce?: (message: string) => void;
  }

  let { client, personID, refreshKey, onSetUp, onAnnounce = () => undefined }: Props = $props();
  let items = $state<AgendaItem[]>([]);
  let loading = $state(true);
  let mutating = $state(false);
  let error = $state('');
  let integrationState = $state('loading');
  let integrationMessage = $state('');
  let truncated = $state(false);
  let title = $state('');
  let listName = $state('agenda');
  let listQuery = $state('');
  const listOptions = $derived(
    [...new Set(['agenda', ...items.map((item) => item.list), listName])]
      .map((name) => ({ name, label: listLabel(name) })),
  );
  let generation = 0;
  let mutationToken = 0;
  let controller: AbortController | null = null;
  let retryRequestID = '';
  let retryFingerprint = '';
  let selectedPersonID: number | undefined;
  let selectedClient: APIClient | undefined;
  let loadedRefreshKey: string | undefined;

  const mutationReady = $derived(integrationState === 'ready');
  const stateMessage = $derived.by(() => {
    if (integrationState === 'loading') return '';
    if (integrationState !== 'ready') {
      return `Kata integration ${integrationState}: ${integrationMessage || 'No status detail was provided.'}`;
    }
    return '';
  });

  $effect(() => {
    const nextPersonID = personID;
    const nextClient = client;
    const nextRefreshKey = refreshKey;
    const pending = mutating;
    untrack(() => {
      const selectionChanged = selectedPersonID !== nextPersonID || selectedClient !== nextClient;
      if (selectionChanged) {
        selectedPersonID = nextPersonID;
        selectedClient = nextClient;
        retryRequestID = '';
        retryFingerprint = '';
        listName = 'agenda';
        // Release the new person's controls while the prior person's write
        // settles. Its token can no longer change this selection's state.
        mutationToken++;
        mutating = false;
      }
      // Same-person revisions keep write ownership and retry protection.
      // Revisit a deferred revision when the pending write releases the form.
      if (selectionChanged || (nextRefreshKey !== loadedRefreshKey && !pending)) {
        items = [];
        error = '';
        integrationState = 'loading';
        integrationMessage = '';
        truncated = false;
        void load(nextPersonID);
      }
    });
  });

  onDestroy(() => controller?.abort());

  async function load(selectedPersonID = personID): Promise<void> {
    // A successful write's reload also covers revisions deferred during it.
    loadedRefreshKey = refreshKey;
    const current = ++generation;
    controller?.abort();
    controller = new AbortController();
    loading = true;
    error = '';
    try {
      const status = await getKataIntegrationStatus({ ...client, signal: controller.signal });
      if (current !== generation) return;
      integrationState = status.data?.state ?? 'unavailable';
      integrationMessage = status.data?.message ?? 'Task integration status is unavailable.';
      // The list endpoint 503s under the same conditions the mutations
      // check, so skip it and let the status message be the single
      // explanation instead of pairing it with a redundant
      // service-unavailable error.
      if (integrationState !== 'ready') return;
      const { data, error: responseError } = await listPersonAgenda(
        { id: selectedPersonID },
        { ...client, signal: controller.signal },
      );
      if (current !== generation) return;
      if (!data) throw new Error(messageFor(responseError, 'Task service is unavailable'));
      items = data.items ?? [];
      truncated = data.truncated;
    } catch (cause) {
      if (current !== generation || controller.signal.aborted) return;
      items = [];
      truncated = false;
      error = cause instanceof Error ? cause.message : 'Task service is unavailable';
    } finally {
      if (current === generation) loading = false;
    }
  }

  // Mirror the server's list canonicalization (trim, collapse internal
  // whitespace runs, lowercase, empty → 'agenda') exactly: the retry
  // fingerprint and the sent payload must both use the canonical form, or a
  // case/spacing change would rotate the idempotency key after an
  // unknown-outcome create and duplicate the task.
  function canonicalList(value: string): string {
    return value.trim().replace(/\s+/g, ' ').toLowerCase() || 'agenda';
  }

  function listLabel(value: string): string {
    return value.charAt(0).toUpperCase() + value.slice(1);
  }

  async function createItem(): Promise<void> {
    const trimmedTitle = title.trim();
    const effectiveList = canonicalList(listName);
    if (!trimmedTitle || mutating || !mutationReady) return;
    // Bind the retry key to the exact submission (person, title, list). An
    // unchanged retry keeps the key so the server can deduplicate an unknown
    // outcome; any payload change must not replay the prior request, so it
    // mints a fresh key instead.
    const fingerprint = `${personID}\u0000${trimmedTitle}\u0000${effectiveList}`;
    if (!retryRequestID || retryFingerprint !== fingerprint) {
      retryRequestID = globalThis.crypto.randomUUID();
      retryFingerprint = fingerprint;
    }
    // Mutation requests carry no abort signal, so the selection can change
    // while one is in flight. Pin the submission to the current load
    // generation: when that generation moves on, drop every outcome — error,
    // title reset, retry-key reset, announcement, reload — so the prior
    // person's result is never written into the new person's view.
    const current = generation;
    const token = ++mutationToken;
    mutating = true;
    error = '';
    try {
      const { data, error: responseError } = await createPersonAgendaItem(
        { id: personID },
        { title: trimmedTitle, list: effectiveList },
        { ...client, headers: { 'Idempotency-Key': retryRequestID } },
      );
      if (current !== generation) return;
      if (!data) throw new Error(messageFor(responseError, 'Unable to add agenda item'));
      title = '';
      retryRequestID = '';
      retryFingerprint = '';
      onAnnounce('Agenda item added.');
      await load();
    } catch (cause) {
      if (current !== generation) return;
      error = cause instanceof Error ? cause.message : 'Unable to add agenda item';
    } finally {
      // Only the newest mutation owns the mutating flag: after a selection
      // change invalidated this token, the new person's state (possibly a
      // newer mutation's) must not be touched here.
      if (token === mutationToken) mutating = false;
    }
  }

  async function unlink(ref: string): Promise<void> {
    if (mutating || !mutationReady) return;
    // Same generation pin as createItem: mutation requests carry no abort
    // signal, so outcomes settled after a selection change must be dropped.
    const current = generation;
    const token = ++mutationToken;
    mutating = true;
    error = '';
    try {
      const { data, error: responseError } = await unlinkPersonAgendaItem(
        { id: personID, ref },
        client,
      );
      if (current !== generation) return;
      if (!data) throw new Error(messageFor(responseError, 'Unable to unlink agenda item'));
      onAnnounce('Agenda item unlinked.');
      await load();
    } catch (cause) {
      if (current !== generation) return;
      error = cause instanceof Error ? cause.message : 'Unable to unlink agenda item';
    } finally {
      // Same token ownership as createItem.
      if (token === mutationToken) mutating = false;
    }
  }

  function messageFor(value: unknown, fallback: string): string {
    return typeof value === 'object' && value !== null && 'message' in value && typeof value.message === 'string'
      ? value.message
      : fallback;
  }
</script>

<section class="person-agenda" aria-labelledby={`person-${personID}-agenda-heading`}>
  <div class="heading"><h3 id={`person-${personID}-agenda-heading`}>Tasks</h3>{#if integrationState === 'disabled' && onSetUp}<Button size="sm" label="Set up" ariaLabel="Set up Tasks" onclick={onSetUp} />{/if}{#if loading}<span role="status">Loading…</span>{/if}</div>
  {#if error}<p class="notice" role="alert">{error}<Button size="sm" label="Retry tasks" onclick={() => void load()} /></p>{/if}
  {#if stateMessage && !(integrationState === 'disabled' && onSetUp)}<div class="heading"><span class="notice" role="status">{stateMessage}</span><Button size="sm" label="Set up" onclick={onSetUp} /></div>{/if}
  {#if !loading && !error && !stateMessage && items.length === 0}<p class="notice">No tasks yet</p>{/if}
  {#if truncated}<p class="notice" role="status">More tasks are linked to this person.</p>{/if}
  {#each items as item (item.uid)}
    <DetailRow>
      <div class="task-row">
        <span class="task-title">{#if item.web_url}<a href={item.web_url} target="_blank" rel="noreferrer">{item.title}</a>{:else}{item.title}{/if}</span>
        {#if item.list !== 'agenda'}<Chip size="xs" uppercase={false} tone="info"><span class="list-label">{listLabel(item.list)}</span></Chip>{/if}
      </div>
      {#snippet actions()}<IconButton size="sm" ariaLabel={`Unlink ${item.title}`} disabled={mutating || !mutationReady} onclick={() => void unlink(item.ref)}><X size={14} /></IconButton>{/snippet}
    </DetailRow>
  {/each}
  {#if mutationReady}
    <form onsubmit={(event) => { event.preventDefault(); void createItem(); }}>
      <TextInput bind:value={title} ariaLabel="New agenda item" placeholder="Add a task…" block disabled={mutating}>
        {#snippet suffix()}
          <div class="list-picker">
            <Typeahead options={listOptions} value={listName} fallbackLabel="Agenda" placeholder="List" allowCustom={!listOptions.some((option) => option.name === listQuery.trim().toLowerCase())} onquery={(query) => { listQuery = query; }} customLabel={'New list "{query}"'} disabled={mutating} onselect={(value) => { listName = canonicalList(value); }}>
              {#snippet trigger({ attachment })}
                <button {@attach attachment} type="button" class="list-trigger" aria-label={`List: ${listLabel(listName)}`} disabled={mutating}>{listLabel(listName)} ▾</button>
              {/snippet}
            </Typeahead>
          </div>
        {/snippet}
      </TextInput>
      <Button type="submit" label="Add" disabled={mutating || !title.trim()} />
    </form>
  {/if}

</section>

<style>
  .person-agenda { display: grid; gap: var(--space-2); min-width: 0; }
  .heading { min-height: var(--control-h); }
  .heading, form { display: flex; align-items: center; justify-content: space-between; gap: var(--control-gap); }
  h3, p { margin: 0; }
  h3 { font-size: var(--font-size-md); }
  form { flex-wrap: wrap; justify-content: flex-start; }
  form :global(.kit-text-input) { min-width: 0; flex: 1; }
  .task-row { display: flex; align-items: baseline; flex-wrap: wrap; gap: var(--field-gap); }
  .task-title { flex: 1 1 12rem; min-width: 0; overflow-wrap: anywhere; }
  .task-row :global(.kit-chip) { box-sizing: border-box; max-width: 100%; margin-left: auto; }
  .list-label { white-space: normal; overflow-wrap: anywhere; }
  .list-picker { max-width: 9rem; --typeahead-panel-min-width: 16rem; }
  .list-trigger { max-width: 100%; padding: 0; border: 0; background: none; color: var(--text-secondary); font: inherit; cursor: pointer; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .list-trigger:disabled { cursor: not-allowed; }
  a { color: var(--link-ink); }
  .notice { color: var(--text-muted); font-size: var(--font-size-sm); }
</style>
