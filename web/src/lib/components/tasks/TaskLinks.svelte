<script lang="ts">
  import { Button } from '@kenn-io/kit-ui';

  import type { APIClient } from '../../api/client';
  import type { components } from '../../api/generated/schema';
  import CreateTaskDialog from './CreateTaskDialog.svelte';

  type Lookup = components['schemas']['TaskLinkLookupResponse'];
  type OutboundMetadata = components['schemas']['TaskLinkOutboundMetadata'];
  type Task = components['schemas']['TaskSummary'];

  let { client, messageId, title, sourceType, sourceIdentifier, onsettings = () => undefined }: {
    client: APIClient; messageId: number; title: string; sourceType: string; sourceIdentifier: string;
    onsettings?: () => void;
  } = $props();

  let loading = $state(true);
  let integrationState = $state('loading');
  let integrationMessage = $state('');
  let indexState = $state('loading');
  let complete = $state(false);
  let reason = $state('');
  let lastScan = $state('');
  let tasks = $state<Task[]>([]);
  let project = $state('');
  let searchQuery = $state('');
  let searchResults = $state<Task[]>([]);
  let searching = $state(false);
  let mutating = $state(false);
  let error = $state('');
  let creating = $state(false);
  let outboundMetadata = $state<OutboundMetadata | null>(null);
  let generation = 0;
  let activeController: AbortController | null = null;
  let existingLinkRetry: { taskID: string; requestID: string; addedAt: string } | undefined;

  $effect(() => {
    const selectedMessageID = messageId;
    const selectedClient = client;
    void selectedClient;
    void loadFor(selectedMessageID);
    return () => activeController?.abort();
  });

  function resetForSelection(): void {
    loading = true; integrationState = 'loading'; integrationMessage = '';
    indexState = 'loading'; complete = false; reason = ''; lastScan = '';
    tasks = []; project = ''; searchQuery = ''; searchResults = []; searching = false;
    mutating = false; error = ''; creating = false; outboundMetadata = null;
    existingLinkRetry = undefined;
  }

  async function loadFor(selectedMessageID: number): Promise<void> {
    const currentGeneration = ++generation;
    activeController?.abort();
    const controller = new AbortController();
    activeController = controller;
    resetForSelection();
    try {
      const status = await client.GET('/api/v1/integrations/tasks/status', { signal: controller.signal });
      if (currentGeneration !== generation) return;
      project = status.data?.project ?? '';
      integrationState = status.data?.state ?? 'unavailable';
      integrationMessage = status.data?.message ?? 'Task integration status is unavailable.';

      // The reverse-index response is independently authoritative and may
      // contain a safe last-good snapshot even when the live integration is down.
      const { data, error: responseError } = await client.GET('/api/v1/messages/{id}/tasks', {
        params: { path: { id: selectedMessageID } }, signal: controller.signal
      });
      if (currentGeneration !== generation) return;
      if (!data) throw new Error(messageFor(responseError, 'Unable to load linked tasks.'));
      apply(data);
    } catch (cause) {
      if (currentGeneration !== generation || controller.signal.aborted) return;
      indexState = 'unavailable'; complete = false;
      error = cause instanceof Error ? cause.message : 'Task integration is unavailable.';
    } finally {
      if (currentGeneration === generation) loading = false;
    }
  }

  function apply(data: Lookup): void {
    indexState = data.state; complete = data.complete; reason = data.reason ?? '';
    lastScan = data.last_scan ?? ''; tasks = data.tasks ?? [];
    outboundMetadata = data.outbound_metadata;
  }

  async function search(): Promise<void> {
    const query = searchQuery.trim();
    if (!query || searching || !mutationReady) return;
    const currentGeneration = generation;
    searching = true; error = ''; searchResults = [];
    try {
      const { data, error: responseError } = await client.GET('/api/v1/integrations/tasks/search', {
        params: { query: { q: query } }, signal: activeController?.signal
      });
      if (currentGeneration !== generation) return;
      if (!data) throw new Error(messageFor(responseError, 'Unable to search tasks.'));
      searchResults = data.tasks ?? [];
    } catch (cause) {
      if (currentGeneration !== generation || activeController?.signal.aborted) return;
      error = cause instanceof Error ? cause.message : 'Unable to search tasks.';
    } finally {
      if (currentGeneration === generation) searching = false;
    }
  }

  async function linkExisting(taskID: string): Promise<void> {
    if (!taskID || mutating || !mutationReady) return;
    const selectedMessageID = messageId;
    const currentGeneration = generation;
    const retry = existingLinkRetry?.taskID === taskID
      ? existingLinkRetry
      : { taskID, requestID: globalThis.crypto.randomUUID(), addedAt: new Date().toISOString() };
    existingLinkRetry = retry;
    mutating = true; error = '';
    try {
      const { data, error: responseError } = await client.POST('/api/v1/messages/{id}/tasks', {
        params: { path: { id: selectedMessageID }, header: { 'X-Request-Id': retry.requestID } },
        body: { task_id: taskID, added_at: retry.addedAt }, signal: activeController?.signal
      });
      if (currentGeneration !== generation) return;
      if (!data) throw new Error(messageFor(responseError, 'Unable to link task.'));
      existingLinkRetry = undefined;
      searchQuery = ''; searchResults = [];
      await loadFor(selectedMessageID);
    } catch (cause) {
      if (currentGeneration !== generation || activeController?.signal.aborted) return;
      error = cause instanceof Error ? cause.message : 'Unable to link task.';
    } finally {
      if (currentGeneration === generation) mutating = false;
    }
  }

  async function unlink(taskID: string): Promise<void> {
    if (!taskID || mutating || !mutationReady) return;
    const selectedMessageID = messageId;
    const currentGeneration = generation;
    mutating = true; error = '';
    try {
      const { data, error: responseError } = await client.DELETE('/api/v1/messages/{id}/tasks/{task_id}', {
        params: { path: { id: selectedMessageID, task_id: taskID } }, signal: activeController?.signal
      });
      if (currentGeneration !== generation) return;
      if (!data) throw new Error(messageFor(responseError, 'Unable to unlink task.'));
      await loadFor(selectedMessageID);
    } catch (cause) {
      if (currentGeneration !== generation || activeController?.signal.aborted) return;
      error = cause instanceof Error ? cause.message : 'Unable to unlink task.';
    } finally {
      if (currentGeneration === generation) mutating = false;
    }
  }

  function messageFor(value: unknown, fallback: string): string {
    return typeof value === 'object' && value !== null && 'message' in value && typeof value.message === 'string'
      ? value.message : fallback;
  }

  // Mutations go through the live task service, so a partial reverse index
  // only limits lookup completeness — it does not block create/link/unlink.
  const mutationReady = $derived(integrationState === 'ready' && (indexState === 'ready' || indexState === 'partial'));
  const degraded = $derived(!mutationReady);
  const partialIndex = $derived(mutationReady && indexState === 'partial');
  const sessionOnlyIndex = $derived(mutationReady && reason === 'cache_persistence_unsupported');
  const stateMessage = $derived(integrationState !== 'ready' && integrationState !== 'loading'
    ? `Task integration ${integrationState}: ${integrationMessage || 'No status detail was provided.'}`
    : reason === 'cache_persistence_unsupported' ? 'Linked-task cache persistence is unavailable on this platform; current-session links may be shown.'
    : indexState === 'authentication_required' ? 'Authentication is required. Cached task titles are hidden until access is restored.'
    : indexState === 'incompatible' ? 'The configured task service is incompatible.'
    : indexState === 'partial' ? `The reverse index is partial${lastScan ? ` (last scanned ${new Date(lastScan).toLocaleString()})` : ''}; an empty result is not authoritative.`
    : indexState === 'stale' ? `The linked-task index is stale${lastScan ? ` (last scanned ${new Date(lastScan).toLocaleString()})` : ''}.`
    : indexState === 'unavailable' ? 'The task service is unavailable; safe cached links may be shown.'
    : indexState === 'disabled' ? 'Task integration is disabled.'
    : indexState === 'not_found' ? 'The configured task service was not found.'
    : indexState === 'wrong_project' ? 'The configured task service has the wrong project or it is unavailable.'
    : reason ? `Task index: ${reason}.` : 'Task integration status is unavailable.');
</script>

<section class="task-links" aria-label="Linked tasks">
  <header><h2>Tasks</h2>{#if mutationReady}<Button size="sm" surface="soft" label="Create task" onclick={() => { creating = true; }} />{/if}</header>
  {#if loading}<p role="status">Loading linked tasks…</p>
  {:else}
    {#if degraded}<div class="degraded" role="alert"><p>{stateMessage} Mutations are disabled.</p><Button size="sm" surface="soft" label="Open Settings" onclick={onsettings} /></div>{/if}
    {#if partialIndex}<p role="alert">{stateMessage}</p>{/if}
    {#if sessionOnlyIndex}<p role="status">Linked-task index updates are available for this session but are not persisted to disk on this platform.</p>{/if}
    {#if tasks.length > 0}
      <ul>{#each tasks as task (task.id)}<li><span>{task.title}</span><Button size="sm" surface="outline" label={`Unlink ${task.title}`} disabled={mutating || !mutationReady} onclick={() => void unlink(task.id)} /></li>{/each}</ul>
    {:else if complete}<p>No linked tasks.</p>{/if}
    {#if mutationReady}
      <form onsubmit={(event) => { event.preventDefault(); void search(); }}>
        <label>Search tasks<input aria-label="Search tasks" bind:value={searchQuery} autocomplete="off" /></label>
        <Button type="submit" size="sm" surface="soft" label="Search" disabled={searching || !searchQuery.trim()} />
      </form>
      {#if searchResults.length > 0}
        <ul aria-label="Task search results">{#each searchResults as task (task.id)}<li><span>{task.title}</span><Button size="sm" surface="soft" label={`Link ${task.title}`} disabled={mutating} onclick={() => void linkExisting(task.id)} /></li>{/each}</ul>
      {/if}
    {/if}
    {#if error}<p role="alert">{error}</p>{/if}
  {/if}
</section>

{#if creating && outboundMetadata}
  <CreateTaskDialog {client} {messageId} {project} defaultTitle={title}
    archiveUID={outboundMetadata.archive_uid} conversationId={outboundMetadata.conversation_id}
    sourceType={outboundMetadata.source_type} sourceIdentifier={outboundMetadata.source_identifier}
    sourceMessageId={outboundMetadata.source_message_id} subject={outboundMetadata.subject}
    from={outboundMetadata.from} sentAt={outboundMetadata.sent_at}
    onclose={() => { creating = false; }} oncreated={() => { creating = false; void loadFor(messageId); }} />
{/if}

<style>
  .task-links { display: grid; gap: var(--space-3); padding: 0 var(--space-5) var(--space-5); }
  header, form, li { display: flex; align-items: center; justify-content: space-between; gap: var(--space-2); }
  h2, p, ul { margin: 0; }
  h2 { font-size: var(--font-size-sm); }
  .degraded { display: flex; align-items: center; justify-content: space-between; gap: var(--space-2); }
  ul { display: grid; gap: var(--space-2); padding: 0; list-style: none; }
  label { display: grid; flex: 1; gap: var(--space-1); color: var(--text-muted); font-size: var(--font-size-xs); }
  input { min-width: 0; padding: var(--space-2); border: 1px solid var(--border-default); border-radius: var(--radius-sm); background: var(--bg-canvas); color: var(--text-primary); }
  [role='alert'] { color: var(--text-muted); font-size: var(--font-size-xs); }
</style>
