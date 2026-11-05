<script lang="ts">
  import { SegmentedControl } from '@kenn-io/kit-ui';
  import { untrack } from 'svelte';
  import type { APIClient } from '../../api/client';
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
  let busy = $state(false);
  const options = $derived([
    ...(email ? [{ value: 'tasks', label: 'Tasks' }] : []),
    ...(kata?.ready ? [{ value: 'kata', label: 'Kata' }] : []),
  ]);
</script>

<Dialog title="Add to task" onclose={() => { if (!busy && !kataBusy) onclose(); }}>
  <div class="task-dialog">
    {#if options.length > 1}
      <div class="provider"><span>Send to</span><SegmentedControl ariaLabel="Send to" disabled={busy || kataBusy} {options} value={provider} onchange={(value) => { provider = value; if (value === 'kata') kataInitialized = true; }} /></div>
    {/if}
    {#if email}<div hidden={provider !== 'tasks'}>
      <TaskLinks {client} {messageId} {title} {onsettings} mode="add" bind:busy oncancel={onclose} oncomplete={onclose} />
    </div>{/if}
    {#if kataInitialized && (email || kata?.ready)}<div hidden={provider !== 'kata'}>
      <KataEvidenceDialog embedded {client} selector={{ kind: 'message', message_id: messageId, start_rune: 0, max_chars: EVIDENCE_WINDOW }} defaultTitle={title} bind:busy={kataBusy} {onclose} />
    </div>{/if}
  </div>
</Dialog>

<style>
  .task-dialog { display: grid; gap: var(--space-6); }
  .provider { display: grid; gap: var(--space-2); color: var(--text-secondary); font-size: var(--font-size-sm); }
</style>
