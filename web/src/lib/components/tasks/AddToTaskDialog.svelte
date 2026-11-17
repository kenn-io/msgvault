<script lang="ts">
  import { SegmentedControl } from '@kenn-io/kit-ui';
  import { untrack } from 'svelte';
  import type { APIClient } from '../../api/client';
  import type { KataIssueReceipt } from '../../api/generated/models';
  import { kataReadiness } from '../../kata/kata-ready.svelte';
  import { EVIDENCE_WINDOW } from '../../kata/evidence';
  import Dialog from '../common/Dialog.svelte';
  import KataEvidenceDialog from '../kata/KataEvidenceDialog.svelte';
  import TaskLinks from './TaskLinks.svelte';

  let { client, messageId, title, email, onclose, onsettings }: {
    client: APIClient; messageId: number; title: string; email: boolean; onclose: () => void; onsettings?: () => void;
  } = $props();
  const kata = kataReadiness();
  let provider = $state(untrack(() => email ? 'tasks' : 'kata'));
  let kataInitialized = $state(untrack(() => !email));
  let kataBusy = $state(false);
  let kataReceipt = $state<KataIssueReceipt>();
  let busy = $state(false);
  let taskAction = $state('create');
  let taskReady = $state(false);
  const options = $derived([
    ...(email ? [{ value: 'tasks', label: 'Tasks' }] : []),
    ...(kata?.ready ? [{ value: 'kata', label: 'Kata' }] : []),
  ]);
</script>

<Dialog title={provider === 'tasks' ? 'Add to task' : 'Add to Kata issue'} onclose={() => { if (!busy && !kataBusy) onclose(); }}>
  <div class="task-dialog">
    {#if !kataReceipt}
      <div class="toolbar">
        {#if options.length > 1}
          <div class="provider"><span>Send to</span><SegmentedControl variant="borderless" ariaLabel="Send to" disabled={busy || kataBusy} {options} value={provider} onchange={(value) => { provider = value; if (value === 'kata') kataInitialized = true; }} /></div>
        {/if}
        {#if provider === 'tasks' && taskReady}
          <SegmentedControl variant="borderless" disabled={busy} ariaLabel="Task" options={[{ value: 'create', label: 'New' }, { value: 'existing', label: 'Existing' }]} value={taskAction} onchange={(value) => taskAction = value} />
        {/if}
      </div>
    {/if}
    {#if email}<div hidden={provider !== 'tasks'}>
      <TaskLinks {client} {messageId} {title} {onsettings} mode="add" bind:action={taskAction} bind:actionReady={taskReady} bind:busy oncancel={onclose} oncomplete={onclose} />
    </div>{/if}
    {#if kataInitialized && (email || kata?.ready)}<div hidden={provider !== 'kata'}>
      <KataEvidenceDialog {client} selector={{ kind: 'message', message_id: messageId, start_rune: 0, max_chars: EVIDENCE_WINDOW }} defaultTitle={title} defaultBrief={`${location.origin}/messages/${messageId}`} modeLabel={email} bind:busy={kataBusy} bind:receipt={kataReceipt} {onclose} />
    </div>{/if}
  </div>
</Dialog>

<style>
  .task-dialog { display: grid; min-width: 0; gap: var(--space-6); }
  .toolbar { display: flex; align-items: center; justify-content: space-between; flex-wrap: wrap; gap: var(--space-4); }
  .provider { display: flex; align-items: center; gap: var(--space-2); color: var(--text-secondary); font-size: var(--font-size-sm); }
</style>
