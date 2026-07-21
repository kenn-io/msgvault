<script lang="ts">
  import { Button, Modal } from '@kenn-io/kit-ui';
  import { untrack } from 'svelte';

  import type { APIClient } from '../../api/client';

  let { client, messageId, project, defaultTitle, archiveUID, conversationId, sourceType, sourceIdentifier,
    sourceMessageId, subject, from, sentAt,
    oncreated = () => undefined, onclose = () => undefined }: {
    client: APIClient; messageId: number; project: string; defaultTitle: string;
    archiveUID: string; conversationId: number; sourceType: string; sourceIdentifier: string;
    sourceMessageId: string; subject: string; from: string; sentAt: string;
    oncreated?: () => void; onclose?: () => void;
  } = $props();

  let title = $state(untrack(() => defaultTitle));
  let description = $state('');
  let priority = $state('');
  let labels = $state('');
  let pending = $state(false);
  let error = $state('');
  let browserRequestID = globalThis.crypto.randomUUID();
  let addedAt = $state(new Date().toISOString());
  let lastAttemptFingerprint = $state<string>();
  const payloadFingerprint = $derived(JSON.stringify(currentPayload()));

  $effect(() => {
    const fingerprint = payloadFingerprint;
    if (lastAttemptFingerprint === undefined || lastAttemptFingerprint === fingerprint) return;
    rotateRetryIdentity();
    lastAttemptFingerprint = undefined;
  });

  async function create(): Promise<void> {
    if (!title.trim() || pending) return;
    const payload = currentPayload();
    const fingerprint = JSON.stringify(payload);
    lastAttemptFingerprint = fingerprint;
    pending = true; error = '';
    try {
      const { data, error: responseError } = await client.POST('/api/v1/messages/{id}/tasks', {
        params: { path: { id: messageId }, header: { 'X-Request-Id': browserRequestID } },
        body: {
          ...payload,
          added_at: addedAt
        }
      });
      if (!data) throw new Error(messageFor(responseError));
      oncreated();
    } catch (cause) {
      error = cause instanceof Error ? cause.message : 'Unable to create task.';
    } finally { pending = false; }
  }

  function rotateRetryIdentity(): void {
    browserRequestID = globalThis.crypto.randomUUID();
    const now = new Date().toISOString();
    addedAt = now > addedAt ? now : new Date(Date.parse(addedAt) + 1).toISOString();
  }

  function parsedLabels(): string[] {
    return labels.split(',').map((label) => label.trim()).filter(Boolean);
  }

  function currentPayload(): {
    title: string;
    description?: string;
    priority?: string;
    labels?: string[];
  } {
    const normalizedLabels = parsedLabels();
    return {
      title: title.trim(),
      ...(description.trim() ? { description: description.trim() } : {}),
      ...(priority ? { priority } : {}),
      ...(normalizedLabels.length ? { labels: normalizedLabels } : {})
    };
  }

  function messageFor(value: unknown): string {
    return typeof value === 'object' && value !== null && 'message' in value && typeof value.message === 'string'
      ? value.message : 'Unable to create task.';
  }
</script>

<Modal title="Create task" onclose={onclose}>
  <form onsubmit={(event) => { event.preventDefault(); void create(); }}>
    <p class="project"><span>Project</span><strong>{project}</strong></p>
    <label>Task title<input aria-label="Task title" bind:value={title} autocomplete="off" /></label>
    <label>Description<textarea aria-label="Description" bind:value={description}></textarea></label>
    <label>Priority<select aria-label="Priority" bind:value={priority}>
      <option value="">Default</option><option value="low">Low</option><option value="normal">Normal</option>
      <option value="high">High</option><option value="urgent">Urgent</option>
    </select></label>
    <label>Labels<input aria-label="Labels" bind:value={labels} placeholder="mail, follow-up" autocomplete="off" /></label>
    <details>
      <summary>Metadata leaving the archive</summary>
      <dl>
        <div><dt>Archive UID</dt><dd>{archiveUID}</dd></div>
        <div><dt>Message ID</dt><dd>{messageId}</dd></div>
        <div><dt>Conversation ID</dt><dd>{conversationId}</dd></div>
        <div><dt>Source type</dt><dd>{sourceType}</dd></div>
        <div><dt>Source identifier</dt><dd>{sourceIdentifier}</dd></div>
        <div><dt>Source message ID</dt><dd>{sourceMessageId}</dd></div>
        <div><dt>Subject snapshot</dt><dd>{subject}</dd></div>
        <div><dt>Sender snapshot</dt><dd>{from}</dd></div>
        <div><dt>Sent at</dt><dd>{sentAt}</dd></div>
        <div><dt>Link added at</dt><dd>{addedAt}</dd></div>
      </dl>
      <p>Bodies and attachments are never sent.</p>
    </details>
    {#if error}<p role="alert">{error}</p>{/if}
    <div class="actions">
      <Button surface="soft" label="Cancel" onclick={onclose} />
      <Button type="submit" tone="info" surface="solid" label="Create task" disabled={pending || !title.trim()} />
    </div>
  </form>
</Modal>

<style>
  form { display: grid; gap: var(--space-4); min-width: min(28rem, 80vw); }
  label { display: grid; gap: var(--space-1); color: var(--text-muted); font-size: var(--font-size-xs); }
  input, textarea, select { padding: var(--space-2); border: 1px solid var(--border-default); border-radius: var(--radius-sm); background: var(--bg-canvas); color: var(--text-primary); }
  textarea { min-height: 6rem; resize: vertical; }
  .project { display: flex; justify-content: space-between; margin: 0; }
  .project span, details { color: var(--text-muted); font-size: var(--font-size-xs); }
  dl { display: grid; gap: var(--space-1); }
  dl div { display: grid; grid-template-columns: 8rem minmax(0, 1fr); gap: var(--space-2); }
  dt { color: var(--text-muted); } dd { margin: 0; overflow-wrap: anywhere; color: var(--text-primary); }
  .actions { display: flex; justify-content: flex-end; gap: var(--space-2); }
</style>
