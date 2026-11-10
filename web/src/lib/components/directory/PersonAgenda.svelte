<script lang="ts">
  import { Button, Chip, IconButton, TextInput, Typeahead } from '@kenn-io/kit-ui';
  import X from '@lucide/svelte/icons/x';
  import { onDestroy, untrack } from 'svelte';
  import DetailRow from '../people/DetailRow.svelte';
  import InlineNotice from '../common/InlineNotice.svelte';
  import EmptyLine from '../common/EmptyLine.svelte';
  import DetailSection from '../people/DetailSection.svelte';
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
  /** The failed operation: its sentence, what the service said for the notice's Details, and how Retry repeats it. */
  let failure = $state<{ sentence: string; details: string; retry: () => Promise<void> } | null>(null);
  let integrationState = $state('loading');
  let integrationMessage = $state('');
  let truncated = $state(false);
  let title = $state('');
  let listName = $state('agenda');
  const listOptions = $derived(
    [...new Set([listName, 'agenda', ...items.map((item) => item.list)])]
      .map((name) => ({ name, label: name })),
  );
  // The add form stays out of the way until the person asks for it.
  let adding = $state(false);
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
        adding = false;
        // Release the new person's controls while the prior person's write
        // settles. Its token can no longer change this selection's state.
        mutationToken++;
        mutating = false;
      }
      // Same-person revisions keep write ownership and retry protection.
      // Revisit a deferred revision when the pending write releases the form.
      if (selectionChanged || (nextRefreshKey !== loadedRefreshKey && !pending)) {
        items = [];
        failure = null;
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
    failure = null;
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
      if (!data) throw new Error(messageFor(responseError, ''));
      items = data.items ?? [];
      truncated = data.truncated;
    } catch (cause) {
      if (current !== generation || controller.signal.aborted) return;
      items = [];
      truncated = false;
      fail("Couldn't load tasks.", cause);
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
    failure = null;
    try {
      const { data, error: responseError } = await createPersonAgendaItem(
        { id: personID },
        { title: trimmedTitle, list: effectiveList },
        { ...client, headers: { 'Idempotency-Key': retryRequestID } },
      );
      if (current !== generation) return;
      if (!data) throw new Error(messageFor(responseError, ''));
      title = '';
      retryRequestID = '';
      retryFingerprint = '';
      onAnnounce('Task added.');
      await load();
    } catch (cause) {
      if (current !== generation) return;
      fail("Couldn't add the task.", cause, createItem);
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
    failure = null;
    try {
      const { data, error: responseError } = await unlinkPersonAgendaItem(
        { id: personID, ref },
        client,
      );
      if (current !== generation) return;
      if (!data) throw new Error(messageFor(responseError, ''));
      onAnnounce('Task unlinked.');
      await load();
    } catch (cause) {
      if (current !== generation) return;
      fail("Couldn't unlink the task.", cause, () => unlink(ref));
    } finally {
      // Same token ownership as createItem.
      if (token === mutationToken) mutating = false;
    }
  }

  function fail(sentence: string, cause: unknown, retry: () => Promise<void> = load): void {
    failure = { sentence, details: cause instanceof Error ? cause.message : '', retry };
  }

  function messageFor(value: unknown, fallback: string): string {
    return typeof value === 'object' && value !== null && 'message' in value && typeof value.message === 'string'
      ? value.message
      : fallback;
  }
</script>

<!-- L2: Add task sits in the header like every other section's add. It opens inline fields, not a dialog, so it
     has no ellipsis; while they're open, the form's Add task and Cancel are the only controls. -->
{#snippet setUp()}<Button label="Set up…" ariaLabel="Set up Tasks…" onclick={onSetUp} />{/snippet}
<DetailSection title="Tasks" addLabel={mutationReady && !adding ? 'Add task' : undefined} onadd={() => adding = true}
  control={integrationState === 'disabled' && onSetUp ? setUp : undefined}>
  {#if loading}<p class="line" role="status">Loading…</p>{/if}
  {#if failure}<InlineNotice message={failure.sentence} details={failure.details || undefined} actionLabel="Retry" onaction={() => void failure?.retry()} />{/if}
  {#if stateMessage && !(integrationState === 'disabled' && onSetUp)}<p class="line state"><span role="status">{stateMessage}</span>{#if onSetUp}<Button label="Set up…" class="text-action" onclick={onSetUp} />{/if}</p>{/if}
  {#if !loading && !failure && !stateMessage && items.length === 0}<EmptyLine message="No tasks yet" />{/if}
  {#if truncated}<p class="line" role="status">More tasks are linked to this person.</p>{/if}
  {#if items.length}<div class="items">
  {#each items as item (item.uid)}
    <DetailRow>
      {#if item.web_url}<a href={item.web_url} target="_blank" rel="noreferrer">{item.title}</a>{:else}{item.title}{/if}
      {#snippet actions()}
        <span class="unlink"><IconButton size="sm" ariaLabel={`Unlink ${item.title}`} disabled={mutating || !mutationReady} onclick={() => void unlink(item.ref)}><X size={14} /></IconButton></span>
        {#if item.list !== 'agenda'}<Chip size="xs" uppercase={false} tone="info" title={item.list}>{item.list}</Chip>{/if}
      {/snippet}
    </DetailRow>
  {/each}
  </div>{/if}
  {#if mutationReady && adding}
    <form data-form onsubmit={(event) => { event.preventDefault(); void createItem(); }}>
      <TextInput bind:value={title} ariaLabel="New task" placeholder="Task" block size="lg" autofocus disabled={mutating}>
        {#snippet suffix()}
          <div class="list-picker">
            <Typeahead options={listOptions} value={listName} fallbackLabel="agenda" placeholder="List" allowCustom disabled={mutating} onselect={(value) => { listName = canonicalList(value); }}>
              {#snippet trigger({ attachment })}
                <button {@attach attachment} type="button" class="list-trigger" aria-label={`List: ${listName}`}><span class="trigger-label">{listName}</span><span aria-hidden="true">▾</span></button>
              {/snippet}
            </Typeahead>
          </div>
        {/snippet}
      </TextInput>
      <div class="form-actions">
        <Button type="submit" label="Add task" disabled={mutating || !title.trim()} />
        <Button label="Cancel" disabled={mutating} onclick={() => { adding = false; title = ''; failure = null; }} />
      </div>
    </form>
  {/if}
</DetailSection>

<style>
  p { margin: 0; }
  /* A status line is Meta, like the empty line; a notice sits 16 from what follows it. */
  .line { color: var(--text-muted); font-size: var(--type-meta-size); line-height: var(--type-meta-line); }
  .state { display: flex; flex-wrap: wrap; align-items: center; column-gap: var(--space-box); }
  :global(.inline-notice) + :is(p, form, .items) { margin-top: var(--space-pane); }
  form { display: flex; flex-wrap: wrap; align-items: center; gap: var(--space-group); padding-block: var(--row-pad-y); }
  form > :global(.kit-text-input:first-child) { min-width: 12rem; flex: 2 1 12rem; }
  .items { display: grid; }
  .form-actions { display: flex; gap: var(--space-group); }
  @media (max-width: 640px) {
    form { row-gap: var(--space-pane); }
    form > :global(.kit-text-input:first-child) { flex-basis: 100%; }
  }
  .items :global(.kit-chip) { max-width: 12rem; min-width: 0; align-self: center; }
  .list-picker { max-width: 9rem; --typeahead-min-width: 0; --typeahead-max-width: 9rem; --typeahead-panel-min-width: 16rem; }
  .list-trigger { display: flex; align-items: center; gap: var(--space-1); max-width: 9rem; padding: 0; border: 0; background: none; color: var(--text-secondary); font: inherit; cursor: pointer; }
  .trigger-label { min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .list-trigger:focus-visible { outline: var(--focus-ring); outline-offset: 1px; }
  .list-trigger:disabled { cursor: not-allowed; }
  .items :global(.actions) { opacity: 1; max-width: 100%; margin-left: auto; }
  .unlink { display: flex; }
  @media (hover: hover) {
    .unlink { opacity: 0; }
    :global(.detail-row:hover) .unlink,
    :global(.detail-row:focus-within) .unlink { opacity: 1; }
  }
  a { color: var(--link-ink); }
</style>
