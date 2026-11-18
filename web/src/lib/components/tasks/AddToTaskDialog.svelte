<script lang="ts">
  import { Button, SegmentedControl } from '@kenn-io/kit-ui';
  import { untrack } from 'svelte';
  import type { APIClient } from '../../api/client';
  import type { KataIssueReceipt, TaskSummary } from '../../api/generated/models';
  import { kataReadiness } from '../../kata/kata-ready.svelte';
  import { EVIDENCE_WINDOW } from '../../kata/evidence';
  import Dialog from '../common/Dialog.svelte';
  import KataEvidenceDialog from '../kata/KataEvidenceDialog.svelte';
  import TaskLinks from './TaskLinks.svelte';

  let { client, messageId, title, email, onclose, onsettings, oncomplete = () => undefined }: {
    client: APIClient; messageId: number; title: string; email: boolean; onclose: () => void; onsettings?: () => void; oncomplete?: () => void;
  } = $props();
  const kata = kataReadiness();
  let provider = $state(untrack(() => email ? 'tasks' : 'kata'));
  let kataInitialized = $state(untrack(() => !email));
  let kataBusy = $state(false);
  let kataReceipt = $state<KataIssueReceipt>();
  let busy = $state(false);
  let taskReceipt = $state<{ action: 'created' | 'linked'; task: TaskSummary }>();
  const options = $derived([
    ...(email ? [{ value: 'tasks', label: 'Tasks' }] : []),
    ...(kata?.ready ? [{ value: 'kata', label: 'Kata' }] : []),
  ]);
</script>

<Dialog title={provider === 'tasks' ? 'Add to task' : 'Add to Kata issue'} onclose={() => { if (!busy && !kataBusy) onclose(); }}>
  <div class="task-dialog">
    {#if options.length > 1 && !kataReceipt && !taskReceipt}
      <div class="provider"><span>Send to</span><SegmentedControl variant="borderless" ariaLabel="Send to" disabled={busy || kataBusy} {options} value={provider} onchange={(value) => { provider = value; if (value === 'kata') kataInitialized = true; }} /></div>
    {/if}
    {#if taskReceipt}
      <div class="receipt"><div role="status"><p>Task {taskReceipt.action}</p><p>{taskReceipt.task.title}</p></div><div class="actions"><Button label="Done" tone="info" surface="solid" onclick={onclose} /></div></div>
    {:else if email}<div hidden={provider !== 'tasks'}>
      <TaskLinks {client} {messageId} {title} {onsettings} mode="add" bind:busy oncancel={onclose} oncomplete={(action, task) => { taskReceipt = { action, task }; oncomplete(); }} />
    </div>{/if}
    {#if kataInitialized && (email || kata?.ready)}<div hidden={provider !== 'kata'}>
      <KataEvidenceDialog {client} selector={{ kind: 'message', message_id: messageId, start_rune: 0, max_chars: EVIDENCE_WINDOW }} defaultTitle={title} defaultBrief={`${location.origin}/messages/${messageId}`} modeLabel={email} bind:busy={kataBusy} bind:receipt={kataReceipt} {onclose} />
    </div>{/if}
  </div>
</Dialog>

<style>
  .task-dialog { display: grid; grid-template-columns: minmax(0, 1fr); gap: var(--space-6); }
  .task-dialog > div { min-width: 0; }
  .receipt, .receipt [role="status"] { display: grid; gap: var(--space-2); }
  .receipt p { margin: 0; }
  .actions { display: flex; justify-content: flex-end; }
  .provider { display: grid; gap: var(--space-2); color: var(--text-secondary); font-size: var(--font-size-sm); }
  .provider :global(.kit-segmented) { justify-self: start; }
</style>
