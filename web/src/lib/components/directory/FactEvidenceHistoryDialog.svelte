<script lang="ts">
  import { Spinner } from '@kenn-io/kit-ui';
  import Dialog from '../common/Dialog.svelte';
  import InlineNotice from '../common/InlineNotice.svelte';
  import EmptyLine from '../common/EmptyLine.svelte';
  import Pager from '../common/Pager.svelte';
  import { formatDateTime, formatShortDate } from '../../util/format';

  import type { FactLedgerController } from '../../directory/fact-ledger-controller.svelte';

  interface Props { controller: FactLedgerController; onClose: () => void }
  let { controller, onClose }: Props = $props();
</script>

<Dialog title="Source history" onclose={onClose} width="medium">
  <div class="history" aria-busy={controller.history.loading}>
    {#if controller.history.loading && controller.history.rows.length === 0}
      <p role="status"><Spinner size={12} label="Loading source history" /> Loading source history…</p>
    {:else}
      {#if controller.history.rows.length === 0 && !controller.history.error}
        <EmptyLine message="No source history yet" />
      {:else}
        <ul aria-label="Source history">
          {#each controller.history.rows as event}
            <li>{event.reasonLabel} · <time datetime={event.createdAt} title={formatDateTime(event.createdAt)}>{formatShortDate(event.createdAt)}</time></li>
          {/each}
        </ul>
      {/if}
      {#if controller.history.error || controller.history.pageError}<InlineNotice message={controller.history.error ?? controller.history.pageError ?? ''} actionLabel="Retry" onaction={() => void controller.retryHistory()} />{/if}
      <Pager offset={controller.history.offset} count={controller.history.rows.length} hasNext={controller.hasHistoryNext}
        disabled={controller.history.loading} label="Source history pages"
        onprevious={() => void controller.previousHistoryPage()} onnext={() => void controller.nextHistoryPage()} />
    {/if}
  </div>
</Dialog>

<style>
  .history { display: grid; gap: var(--space-group); min-width: 0; font-size: var(--type-body-size); line-height: var(--type-body-line); }
  ul, p { margin: 0; }
  ul { display: grid; min-width: 0; padding: 0; list-style: none; }
  li { min-width: 0; padding: var(--row-pad-y) 0; box-shadow: inset 0 -1px 0 var(--border-muted); overflow-wrap: anywhere; }
  .history > :global(.inline-notice) { margin-top: var(--space-group); }
  p[role='status'] { display: flex; align-items: center; gap: var(--space-group); color: var(--text-muted); }
  time { white-space: nowrap; }
</style>
