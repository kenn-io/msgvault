<script lang="ts">
  import { createBrowserRequestID } from '../../api/request-id';
  import { createOrLinkMessageTask as generatedCreateOrLinkMessageTask } from '../../api/generated/api/api';
  import { Chip, Button, SelectDropdown, TextInput } from '@kenn-io/kit-ui';
  import { untrack } from 'svelte';
  import TaskMetadataDisclosure from './TaskMetadataDisclosure.svelte';
  import { formatShortDate } from '../../util/format';
  import type { APIClient } from '../../api/client';
  let {
    client,
    pending = $bindable(false),
    disabled = false,
    messageId,
    project,
    defaultTitle,
    archiveUID,
    conversationId,
    sourceType,
    sourceIdentifier,
    sourceMessageId,
    subject,
    from,
    sentAt,
    oncreated = () => undefined,
    onclose = () => undefined,
  }: {
    client: APIClient;
    pending?: boolean;
    disabled?: boolean;
    messageId: number;
    project: string;
    defaultTitle: string;
    archiveUID: string;
    conversationId: number;
    sourceType: string;
    sourceIdentifier: string;
    sourceMessageId: string;
    subject: string;
    from: string;
    sentAt: string;
    oncreated?: () => void;
    onclose?: () => void;
  } = $props();
  let title = $state(untrack(() => defaultTitle));
  let description = $state('');
  let priority = $state('');
  let labels = $state('');
  let error = $state('');
  let browserRequestID = createBrowserRequestID();
  let addedAt = $state(new Date().toISOString());
  let lastAttemptFingerprint = $state<string>();
  const messageURL = $derived(`${location.origin}/messages/${messageId}`);
  const contextParts = $derived([subject.trim(), from.trim(), sentAt.trim() ? formatShortDate(sentAt) : ''].filter(Boolean));
  const fullContext = $derived(contextParts.join(' · ') || defaultTitle);
  const priorityOptions = [
    { value: '', label: 'No priority' },
    { value: 'low', label: 'Low' },
    { value: 'normal', label: 'Normal' },
    { value: 'high', label: 'High' },
    { value: 'urgent', label: 'Urgent' },
  ];
  const payloadFingerprint = $derived(JSON.stringify(currentPayload()));
  $effect(() => {
    const fingerprint = payloadFingerprint;
    if (lastAttemptFingerprint === undefined || lastAttemptFingerprint === fingerprint) return;
    rotateRetryIdentity();
    lastAttemptFingerprint = undefined;
  });
  async function create(): Promise<void> {
    if (!title.trim() || pending || disabled) return;
    const payload = currentPayload();
    const fingerprint = JSON.stringify(payload);
    lastAttemptFingerprint = fingerprint;
    pending = true;
    error = '';
    try {
      const { data, error: responseError } = await generatedCreateOrLinkMessageTask(
        { id: messageId },
        {
          ...payload,
          added_at: addedAt,
        },
        {
          ...client,
          headers: { 'X-Request-Id': browserRequestID },
        },
      );
      if (!data) throw new Error(messageFor(responseError));
      oncreated();
    } catch (cause) {
      error = cause instanceof Error ? cause.message : 'Unable to create task.';
    } finally {
      pending = false;
    }
  }
  function rotateRetryIdentity(): void {
    browserRequestID = createBrowserRequestID();
    const now = new Date().toISOString();
    addedAt = now > addedAt ? now : new Date(Date.parse(addedAt) + 1).toISOString();
  }
  function parsedLabels(): string[] {
    return labels
      .split(',')
      .map((label) => label.trim())
      .filter(Boolean);
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
      description: [description.trim(), messageURL].filter(Boolean).join('\n\n'),
      ...(priority ? { priority } : {}),
      ...(normalizedLabels.length ? { labels: normalizedLabels } : {}),
    };
  }
  function messageFor(value: unknown): string {
    return typeof value === 'object' && value !== null && 'message' in value && typeof value.message === 'string'
      ? value.message
      : 'Unable to create task.';
  }
  function requestClose(): void {
    if (pending) return;
    onclose();
  }
</script>

  <form
    aria-busy={pending}
    onsubmit={(event) => {
      event.preventDefault();
      void create();
    }}
  >
    <p class="project"><span>Project</span><strong>{project}</strong></p>
    <div class="message"><span>Message</span><Chip tone="neutral" uppercase={false} ariaLabel={fullContext} title={fullContext}><span class="context">{fullContext}</span></Chip></div>
    <label>Task title<TextInput ariaLabel="Task title" bind:value={title} autocomplete="off" block /></label>
    <label>Description<textarea aria-label="Description" bind:value={description}></textarea></label>
    <label
      >Priority<SelectDropdown
        title="Priority"
        value={priority}
        options={priorityOptions}
        onchange={(value) => {
          priority = value;
        }}
      /></label
    >
    <label
      >Labels<TextInput
        ariaLabel="Labels"
        bind:value={labels}
        placeholder="mail, follow-up"
        autocomplete="off"
        block
      /></label
    >
    <TaskMetadataDisclosure {messageURL} {addedAt} {archiveUID} {messageId} {conversationId} {sourceType} {sourceIdentifier} {sourceMessageId} {subject} {from} {sentAt} />
    <p class="disclosure">Bodies and attachments are never sent.</p>
    {#if error}<p role="alert">{error}</p>{/if}
    <div class="actions">
      <Button surface="soft" label="Cancel" disabled={pending} onclick={requestClose} />
      <Button type="submit" tone="info" surface="solid" label="Create task" disabled={pending || disabled || !title.trim()} />
    </div>
  </form>

<style>
  .message { display: grid; grid-template-columns: minmax(0, 1fr); min-width: 0; justify-items: start; gap: var(--space-2); color: var(--text-secondary); font-size: var(--font-size-sm); }
  form {
    --field-text-size: 16px;
    display: grid;
    gap: var(--space-6);
    min-width: 0;
  }
  form :global(.kit-chip) { justify-self: start; max-width: 100%; font-weight: 400; line-height: var(--type-meta-line); }
  .context { display: block; line-height: var(--type-meta-line); white-space: normal; overflow-wrap: anywhere; }
  .disclosure { margin: 0; color: var(--text-secondary); font-size: 12px; }
  label {
    display: grid;
    gap: var(--space-2);
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }
  label :global(.kit-text-input__control), label :global(.kit-select-dropdown__trigger) { font-size: 16px; }
  textarea {
    min-height: 6rem;
    resize: vertical;
    padding: var(--space-2);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    background: var(--bg-canvas);
    color: var(--text-primary);
  }
  .project { display: grid; gap: var(--space-2); margin: 0; }
  .project span { color: var(--text-secondary); font-size: var(--font-size-sm); }
  .actions {
    display: flex;
    justify-content: flex-end;
    gap: var(--space-4);
  }
</style>
