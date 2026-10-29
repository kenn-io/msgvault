<script lang="ts">
  import { Button } from '@kenn-io/kit-ui';
  import type { APIClient } from '../../api/client';
  import type { MeetingRef } from '../../api/generated/models';
  import type { MeetingsController } from '../../meetings/controller.svelte';
  import Dialog from '../common/Dialog.svelte';
  import MeetingActions from './MeetingActions.svelte';
  import MeetingActionFilters from './MeetingActionFilters.svelte';
  import MeetingMetrics from './MeetingMetrics.svelte';

  let { client, controller, onclose, onOpenMeeting, onReload }: {
    client: APIClient; controller: MeetingsController; onclose: () => void; onOpenMeeting?: (meeting: MeetingRef) => void; onReload: () => void;
  } = $props();
</script>

<Dialog title="Action items" {onclose}>
  <div class="actions-dialog">
    <MeetingActionFilters compact {controller} />
    {#if controller.metrics}
      <details><summary>Meeting metrics</summary><MeetingMetrics metrics={controller.metrics} /></details>
    {/if}
    {#if controller.actionsError}
      <p role="alert">{controller.actionsError.message}</p>
      {#if controller.actionsError.recovery === 'reload' || controller.actionsError.recovery === 'retry'}<Button label="Reload meeting activity" onclick={onReload} />{/if}
    {/if}
    {#if controller.actions}<MeetingActions compact {client} evidence={controller.actions} {onOpenMeeting} />{/if}
    {#if controller.actionsLoading}<p role="status">Loading action items…</p>{/if}
    {#if controller.actions?.next_cursor && !controller.actionsError}<Button label="Show more" disabled={controller.actionsLoading} onclick={() => void controller.loadMore()} />{/if}
  </div>
</Dialog>

<style>
  .actions-dialog { display: grid; gap: var(--space-4); }
  details { font-size: var(--font-size-sm); }
  summary { cursor: pointer; margin-bottom: var(--space-3); }
  p { margin: 0; font-size: var(--font-size-sm); }
</style>
