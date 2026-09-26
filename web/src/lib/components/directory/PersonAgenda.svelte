<script lang="ts">
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
    onAnnounce?: (message: string) => void;
  }

  let { client, personID, onAnnounce = () => undefined }: Props = $props();
  let items = $state<AgendaItem[]>([]);
  let loading = $state(true);
  let mutating = $state(false);
  let error = $state('');
  let integrationState = $state('loading');
  let integrationMessage = $state('');
  let truncated = $state(false);
  let title = $state('');
  let listName = $state('agenda');
  let generation = 0;
  let mutationToken = 0;
  let controller: AbortController | null = null;
  let retryRequestID = '';
  let retryFingerprint = '';

  const groups = $derived.by(() => {
    const grouped = new Map<string, AgendaItem[]>();
    for (const item of items) grouped.set(item.list, [...(grouped.get(item.list) ?? []), item]);
    return [...grouped.entries()];
  });

  const mutationReady = $derived(integrationState === 'ready');
  const stateMessage = $derived.by(() => {
    if (integrationState === 'loading') return '';
    if (integrationState !== 'ready') {
      return `Kata integration ${integrationState}: ${integrationMessage || 'No status detail was provided.'}`;
    }
    return '';
  });

  $effect(() => {
    const selectedPersonID = personID;
    void client;
    // Selection changed: the prior person's visible state must not survive
    // into the transition. Clear it synchronously so no Unlink or Add can
    // pair old data with the new selection while the fresh status and list
    // resolve; load() restores each piece only for the current generation.
    items = [];
    error = '';
    integrationState = 'loading';
    integrationMessage = '';
    truncated = false;
    retryRequestID = '';
    retryFingerprint = '';
    // Mutation requests carry no abort signal, so one may still be in flight
    // when the selection changes. Invalidate its ownership of the mutating
    // flag and release this person's controls immediately instead of leaving
    // them disabled until a request that may never settle returns; the stale
    // completion handler no longer matches the token, so it can neither clear
    // a newer mutation's state nor unlock mid-flight.
    mutationToken++;
    mutating = false;
    void load(selectedPersonID);
    return () => controller?.abort();
  });

  async function load(selectedPersonID = personID): Promise<void> {
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

  function label(value: string): string {
    return value.replace(/\b\w/g, (letter) => letter.toUpperCase());
  }
</script>

<section class="person-agenda" aria-labelledby={`person-${personID}-agenda-heading`}>
  <div class="heading">
    <div>
      <h3 id={`person-${personID}-agenda-heading`}>Agenda</h3>
      <p>Open tasks linked to this person. Edit and complete them in Kata.</p>
    </div>
    {#if loading}<span role="status">Loading…</span>{/if}
  </div>

  {#if error}<p class="notice" role="status">{error}</p>{/if}

  {#if stateMessage}<p class="notice" role="alert">{stateMessage}</p>{/if}

  {#if !loading && !error && !stateMessage && items.length === 0}
    <p class="notice">No linked Kata items.</p>
  {/if}

  {#if truncated}<p class="notice" role="status">More open tasks are linked to this person. View the full list in Kata.</p>{/if}

  {#each groups as [list, listItems]}
    <div class="list">
      <h4>{label(list)}</h4>
      <ul>
        {#each listItems as item (item.uid)}
          <li>
            <span>
              <strong>{item.title}</strong>
              {#if item.state !== 'open'}<small>{label(item.state)}</small>{/if}
              {#if item.status !== item.state}<small>{label(item.status)}</small>{/if}
              {#if item.web_url}<a href={item.web_url} target="_blank" rel="noreferrer">Open in Kata</a>{/if}
            </span>
            <button type="button" disabled={mutating || !mutationReady} onclick={() => void unlink(item.ref)} aria-label={`Unlink ${item.title}`}>Unlink</button>
          </li>
        {/each}
      </ul>
    </div>
  {/each}

  <form onsubmit={(event) => { event.preventDefault(); void createItem(); }}>
    <label>New agenda item <input bind:value={title} disabled={mutating || !mutationReady} /></label>
    <label>List <input bind:value={listName} disabled={mutating || !mutationReady} /></label>
    <button type="submit" disabled={mutating || !mutationReady || !title.trim()}>Add item</button>
  </form>
</section>

<style>
  .person-agenda { display: grid; gap: var(--space-3); }
  .heading, li, form { display: flex; align-items: center; justify-content: space-between; gap: var(--space-3); }
  h3, h4, p, ul { margin: 0; }
  h4 { font-size: var(--font-size-sm); color: var(--text-secondary); }
  ul { list-style: none; padding: 0; display: grid; gap: var(--space-2); }
  li { padding: var(--space-2); border: 1px solid var(--border-default); border-radius: var(--radius-sm); }
  li span { display: flex; align-items: baseline; gap: var(--space-2); }
  small { color: var(--text-muted); }
  form { flex-wrap: wrap; justify-content: flex-start; }
  label { display: grid; gap: var(--space-1); color: var(--text-secondary); font-size: var(--font-size-sm); }
  input { min-width: 12rem; }
  .notice { color: var(--text-secondary); }
</style>
