<script lang="ts">
  import { onDestroy, untrack } from 'svelte';
  import { Button, SelectDropdown, TextInput } from '@kenn-io/kit-ui';
  import type { APIClient } from '../../api/client';
  import type { MeetingActionsRequest, MeetingRef } from '../../api/generated/models';
  import type { MeetingsController } from '../../meetings/controller.svelte';
  import Dialog from '../common/Dialog.svelte';
  import MeetingActions from './MeetingActions.svelte';
  import MeetingMetrics from './MeetingMetrics.svelte';

  let { client, controller, onclose, onOpenMeeting, onReload }: {
    client: APIClient; controller: MeetingsController; onclose: () => void; onOpenMeeting?: (meeting: MeetingRef) => void; onReload: () => void;
  } = $props();
  let status = $state(untrack(() => controller.filters.status ?? ''));
  let assignee = $state(untrack(() => controller.filters.assigneeEmail ?? ''));
  let assigneeTimer: ReturnType<typeof setTimeout> | undefined;
  onDestroy(() => clearTimeout(assigneeTimer));
  function apply(): void { clearTimeout(assigneeTimer); void controller.setFilters({ status: (status || undefined) as MeetingActionsRequest['status'], assigneeEmail: assignee }); }
  const statuses = [{ value: '', label: 'All statuses' }, { value: 'pending', label: 'Pending' }, { value: 'completed', label: 'Completed' }, { value: 'cancelled', label: 'Cancelled' }, { value: 'unknown', label: 'Unknown' }];
</script>

<Dialog title="Action items" {onclose}>
  <div class="actions-dialog">
    <div class="filters" aria-label="Action item filters">
      <SelectDropdown title="Status" options={statuses} value={status} onchange={(value) => { status = value; apply(); }} />
      <TextInput ariaLabel="Assignee" placeholder="Assignee email" value={assignee} oninput={(value) => { assignee = value; clearTimeout(assigneeTimer); assigneeTimer = setTimeout(apply, 300); }} block />
    </div>
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
  .filters { display: grid; grid-template-columns: 1fr 1fr; gap: var(--space-2); }
  details { font-size: var(--font-size-sm); }
  summary { cursor: pointer; margin-bottom: var(--space-3); }
  p { margin: 0; font-size: var(--font-size-sm); }
</style>
