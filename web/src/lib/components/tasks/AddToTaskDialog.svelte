<script lang="ts">
  import { Button, SelectDropdown } from '@kenn-io/kit-ui';
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
  let quoteOpen = $state(false);
  let busy = $state(false);
  const options = $derived([
    ...(email ? [{ value: 'tasks', label: 'Tasks' }] : []),
    ...(kata?.ready ? [{ value: 'kata', label: 'Kata' }] : []),
  ]);
</script>

{#if quoteOpen}
  <KataEvidenceDialog {client} selector={{ kind: 'message', message_id: messageId, start_rune: 0, max_chars: EVIDENCE_WINDOW }} defaultTitle={title} {onclose} />
{:else}
  <Dialog title="Add to task" onclose={() => { if (!busy) onclose(); }}>
    <div class="task-dialog">
      {#if options.length > 1}<SelectDropdown title="Provider" disabled={busy} options={options} value={provider} onchange={(value) => provider = value} />{/if}
      {#if provider === 'tasks' && email}
        <TaskLinks {client} {messageId} {title} sourceType="" sourceIdentifier="" {onsettings} mode="add" bind:busy oncomplete={onclose} />
      {:else if kata?.ready}
        <Button label="Add quote to Kata…" onclick={() => quoteOpen = true} />
      {:else}
        <p>Tasks are unavailable.</p>
        <Button label="Open Settings" onclick={onsettings} />
      {/if}
    </div>
  </Dialog>
{/if}

<style>
  .task-dialog { display: grid; gap: var(--space-4); }
</style>
