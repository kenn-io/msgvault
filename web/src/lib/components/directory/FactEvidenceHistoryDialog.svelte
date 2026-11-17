<script lang="ts">
  import { appShortcuts, Button, Modal, Spinner } from '@kenn-io/kit-ui';
  import { onDestroy, onMount } from 'svelte';

  import type { FactLedgerController } from '../../directory/fact-ledger-controller.svelte';

  interface Props { controller: FactLedgerController; onClose: () => void }
  let { controller, onClose }: Props = $props();
  let releaseShortcutScope: (() => void) | undefined;

  onMount(() => { releaseShortcutScope = appShortcuts.pushScope('fact-evidence-history'); });
  onDestroy(() => releaseShortcutScope?.());
</script>

<Modal
  title="Evidence support history"
  ariaLabel="Evidence support history"
  closeLabel="Close evidence support history"
  onclose={onClose}
  maxWidth="min(640px, calc(100vw - 32px))"
>
  <div class="history" aria-busy={controller.history.loading}>
    {#if controller.history.loading && controller.history.rows.length === 0}
      <p role="status"><Spinner size={12} label="Loading evidence support history" /> Loading support history…</p>
    {:else if controller.history.error && controller.history.rows.length === 0}
      <div class="message" role="alert">
        <p>{controller.history.error}</p>
        <Button label="Retry support history" size="sm" onclick={() => void controller.retryHistory()} />
      </div>
    {:else}
      {#if controller.history.pageError}
        <div class="message" role="alert">
          <p>{controller.history.pageError}</p>
          <Button label="Retry support history" size="sm" onclick={() => void controller.retryHistory()} />
        </div>
      {/if}
      {#if controller.history.rows.length === 0}
        <p>No support-history events on this page.</p>
      {:else}
        <ul aria-label="Evidence support events">
          {#each controller.history.rows as event}
            <li>
              <strong>{event.supported ? 'Supported' : 'Unsupported'}</strong>
              <span>{event.reasonLabel}</span>
              <time datetime={event.createdAt}>{event.createdAt}</time>
            </li>
          {/each}
        </ul>
      {/if}
      <nav aria-label="Evidence support-history pages">
        <Button label="First support-history page" size="sm" disabled={controller.history.loading || controller.history.offset === 0} onclick={() => void controller.firstHistoryPage()} />
        <Button label="Previous support-history page" size="sm" disabled={controller.history.loading || controller.history.offset === 0} onclick={() => void controller.previousHistoryPage()} />
        <span>Offset {controller.history.offset}</span>
        <Button label="Next support-history page" size="sm" disabled={controller.history.loading || !controller.hasHistoryNext} onclick={() => void controller.nextHistoryPage()} />
      </nav>
    {/if}
  </div>
  {#snippet footer()}<Button label="Close" onclick={onClose} />{/snippet}
</Modal>

<style>
  .history, ul, li, .message { display: grid; gap: var(--space-3); }
  .history { min-width: min(32rem, calc(100vw - 64px)); }
  ul, p { margin: 0; }
  ul { padding-left: var(--space-5); }
  li { gap: var(--space-1); overflow-wrap: anywhere; }
  li span, li time, nav { color: var(--text-muted); font-size: var(--font-size-sm); }
  nav { display: flex; align-items: center; justify-content: center; gap: var(--space-3); flex-wrap: wrap; }
  .message { justify-items: start; }
</style>
