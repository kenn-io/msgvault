<script lang="ts">
  import { Button, SelectDropdown } from '@kenn-io/kit-ui';
  import { onDestroy, untrack } from 'svelte';
  import type { APIClient } from '../../api/client';
  import { kataReadiness } from '../../kata/kata-ready.svelte';
  import { EVIDENCE_WINDOW } from '../../kata/evidence';
  import Dialog from '../common/Dialog.svelte';
  import KataEvidenceDialog from '../kata/KataEvidenceDialog.svelte';
  import TaskLinks from './TaskLinks.svelte';

  let { client, messageId, title, email, onclose, onsettings, onchanged }: {
    client: APIClient; messageId: number; title: string; email: boolean; onclose: () => void; onsettings?: () => void; onchanged?: () => void;
  } = $props();
  const kata = kataReadiness();
  let provider = $state(untrack(() => email ? 'tasks' : 'kata'));
  let quoteOpen = $state(false);
  let busy = $state(false);
  let changed = false;
  onDestroy(() => { if (changed) onchanged?.(); });
  const options = $derived([
    ...(email ? [{ value: 'tasks', label: 'Tasks' }] : []),
    ...(kata?.ready ? [{ value: 'kata', label: 'Kata' }] : []),
  ]);
</script>

{#if quoteOpen || (!email && kata?.ready)}
  <KataEvidenceDialog {client} selector={{ kind: 'message', message_id: messageId, start_rune: 0, max_chars: EVIDENCE_WINDOW }} defaultTitle={title} {onclose} oncreated={() => changed = true} />
{:else}
  <Dialog title="Add to task" onclose={() => { if (!busy) onclose(); }}>
    <div class="task-dialog">
      {#if options.length > 1}<SelectDropdown title="Provider" disabled={busy} options={options} value={provider} onchange={(value) => provider = value} />{/if}
      {#if email}<div hidden={provider !== 'tasks'}>
        <TaskLinks {client} {messageId} {title} {onsettings} mode="add" bind:busy oncancel={onclose} oncomplete={() => { changed = true; onclose(); }} />
      </div>{/if}
      {#if provider !== 'tasks'}
        <Button label="Add quote to Kata…" onclick={() => quoteOpen = true} />
      {/if}
    </div>
  </Dialog>
{/if}

<style>
  .task-dialog { display: grid; gap: var(--space-4); }
</style>
