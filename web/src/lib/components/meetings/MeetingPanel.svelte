<script lang="ts">
  import { Button, SelectDropdown, TextInput } from '@kenn-io/kit-ui';
  import { onDestroy, untrack } from 'svelte';
  import type { APIClient } from '../../api/client';
  import type { MeetingActionsRequest, MeetingRef } from '../../api/generated/models';
  import { canonicalFingerprint } from '../../explore/selection';
  import { createMeetingsAPI } from '../../meetings/api';
  import { MeetingsController, type MeetingPanelScope } from '../../meetings/controller.svelte';
  import MeetingActions from './MeetingActions.svelte';
  import MeetingMetrics from './MeetingMetrics.svelte';

  let { client, scope, refreshKey = '', onReloadScope = undefined, onOpenMeeting = undefined }: {
    client: APIClient;
    scope: MeetingPanelScope;
    /** UI invalidation only; the server still resolves the durable identity. */
    refreshKey?: string;
    /** Explore owners must reload the corresponding result before publishing new authority. */
    onReloadScope?: () => void;
    onOpenMeeting?: (meeting: MeetingRef) => void;
  } = $props();
  const controller = new MeetingsController(createMeetingsAPI(untrack(() => client)));
  const scopeFingerprint = $derived(canonicalFingerprint({ scope, refreshKey }));
  let sourceStatus = $state('');
  let assigneeEmail = $state('');
  const statusOptions = [ { value: '', label: 'All source statuses' },
    { value: 'pending', label: 'Pending' }, { value: 'completed', label: 'Completed' },
    { value: 'cancelled', label: 'Cancelled' }, { value: 'unknown', label: 'Unknown' } ];
  const errors = $derived.by(() => {
    const failures = [controller.metricsError, controller.actionsError].filter((error) => error !== undefined);
    return failures.filter((error, index) => failures.findIndex((candidate) => candidate.message === error.message) === index);
  });
  const canReload = $derived(errors.some((error) => error.recovery === 'reload' || error.recovery === 'retry'));

  $effect.pre(() => {
    void scopeFingerprint;
    untrack(() => { void controller.setScope(scope, refreshKey); });
  });
  onDestroy(() => controller.destroy());

  function reload(): void {
    if (scope.kind === 'explore' && onReloadScope) onReloadScope();
    else void controller.reload();
  }

  function applyFilters(event: SubmitEvent): void {
    event.preventDefault();
    void controller.setFilters({ status: (sourceStatus || undefined) as MeetingActionsRequest['status'], assigneeEmail });
  }
</script>

<section class="meeting-panel" aria-label="Meeting activity">
  <header><h2>Meeting activity and follow-ups</h2></header>
  {#if controller.metricsLoading}<p role="status">Loading meeting metrics…</p>{/if}
  {#each errors as error (error.message)}<p role="alert">{error.message}</p>{/each}
  {#if canReload}<Button label="Reload meeting activity" size="sm" surface="outline" onclick={reload} />{/if}
  {#if controller.metrics}<MeetingMetrics metrics={controller.metrics} />{/if}
  <form class="action-filters" onsubmit={applyFilters}>
    <SelectDropdown title="Source status" value={sourceStatus} options={statusOptions} onchange={(value) => (sourceStatus = value)} />
    <TextInput value={assigneeEmail} ariaLabel="Assignee email" placeholder="Assignee email" oninput={(value) => (assigneeEmail = value)} />
    <Button type="submit" label="Apply action filters" size="sm" surface="outline" />
  </form>
  {#if controller.actions}
    <p>{controller.actions.total_count.toLocaleString()} matching action items</p>
    <MeetingActions {client} evidence={controller.actions} {onOpenMeeting} />
    {#if controller.actions.next_cursor && !controller.actionsError}
      <Button label="Load more action items" size="sm" surface="outline" disabled={controller.actionsLoading} onclick={() => void controller.loadMore()} />
    {/if}
  {/if}
  {#if controller.actionsLoading}<p role="status">Loading action evidence…</p>{/if}
</section>

<style>
  .meeting-panel { display: grid; gap: var(--space-3); padding: var(--space-4); min-width: 0; }
  h2, p { margin: 0; font-size: var(--font-size-sm); color: var(--text-secondary); }
  h2 { color: var(--text-primary); }
  .action-filters { display: flex; flex-wrap: wrap; gap: var(--space-2); align-items: center; }
</style>
