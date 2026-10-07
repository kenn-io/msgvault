<script lang="ts">
  import { Button, Card, SelectDropdown, TextInput } from '@kenn-io/kit-ui';
  import { onDestroy, untrack } from 'svelte';
  import type { APIClient } from '../../api/client';
  import type { MeetingActionsRequest, MeetingRef } from '../../api/generated/models';
  import { canonicalFingerprint } from '../../explore/selection';
  import { createMeetingsAPI } from '../../meetings/api';
  import { MeetingsController, type MeetingPanelScope } from '../../meetings/controller.svelte';
  import MeetingActions from './MeetingActions.svelte';
  import MeetingMetrics from './MeetingMetrics.svelte';

  let { client, scope, refreshKey = '', showHeading = true, onReloadScope = undefined, onOpenMeeting = undefined }: {
    client: APIClient;
    scope: MeetingPanelScope;
    /** UI invalidation only; the server still resolves the durable identity. */
    refreshKey?: string;
    showHeading?: boolean;
    /** Explore owners must reload the corresponding result before publishing new authority. */
    onReloadScope?: () => void;
    onOpenMeeting?: (meeting: MeetingRef) => void;
  } = $props();
  const controller = new MeetingsController(createMeetingsAPI(untrack(() => client)));
  const headingID = $props.id();
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

<Card level="default" padding="none" class="meeting-panel-card">
  <section
    class="meeting-panel"
    class:meeting-panel--compact={!showHeading}
    aria-label="Meeting activity"
  >
    {#if showHeading}
      <header class="panel-heading">
        <span class="eyebrow">Meetings</span>
        <h2 id={headingID}>Meeting activity and follow-ups</h2>
      </header>
    {/if}

    <div class="overview-section">
      {#if controller.metricsLoading}<p class="state" role="status">Loading meeting metrics…</p>{/if}
      {#each errors as error (error.message)}<p class="state state--error" role="alert">{error.message}</p>{/each}
      {#if canReload}<Button label="Reload meeting activity" size="sm" surface="outline" onclick={reload} />{/if}
      {#if controller.metrics}<MeetingMetrics metrics={controller.metrics} />{/if}
    </div>

    <section class="actions-section" aria-labelledby={`${headingID}-actions`}>
      <header class="section-heading">
        <div>
          <span class="eyebrow">Follow-ups</span>
          <h3 id={`${headingID}-actions`}>Action items</h3>
        </div>
        {#if controller.actions}
          <span class="result-count">{controller.actions.total_count.toLocaleString()} matching</span>
        {/if}
      </header>

      <Card level="inset" padding="sm">
        <form class="action-filters" aria-label="Action item filters" onsubmit={applyFilters}>
          <div class="filter-field">
            <span>Status</span>
            <SelectDropdown title="Source status" value={sourceStatus} options={statusOptions} onchange={(value) => (sourceStatus = value)} />
          </div>
          <div class="filter-field filter-field--assignee">
            <span>Assignee</span>
            <TextInput value={assigneeEmail} ariaLabel="Assignee email" placeholder="Email address" block oninput={(value) => (assigneeEmail = value)} />
          </div>
          <Button type="submit" label="Apply action filters" size="sm" surface="outline" />
        </form>
      </Card>

      {#if controller.actions}
        <MeetingActions {client} evidence={controller.actions} {onOpenMeeting} />
        {#if controller.actions.next_cursor && !controller.actionsError}
          <Button label="Load more action items" size="sm" surface="outline" disabled={controller.actionsLoading} onclick={() => void controller.loadMore()} />
        {/if}
      {/if}
      {#if controller.actionsLoading}<p class="state" role="status">Loading action evidence…</p>{/if}
    </section>
  </section>
</Card>

<style>
  :global(.meeting-panel-card) { width: 100%; max-width: 100%; min-width: 0; overflow: hidden; }
  .meeting-panel, .overview-section, .actions-section { min-width: 0; }
  .meeting-panel { display: grid; }
  .panel-heading { padding: var(--space-5) var(--space-6) var(--space-4); border-bottom: 1px solid var(--border-muted); }
  .overview-section, .actions-section { display: grid; gap: var(--space-4); padding: var(--space-5) var(--space-6); }
  .actions-section { border-top: 1px solid var(--border-muted); }
  .meeting-panel--compact .overview-section { padding-top: var(--space-5); }
  .panel-heading, .section-heading > div { display: grid; gap: var(--space-1); }
  .section-heading { display: flex; align-items: end; justify-content: space-between; gap: var(--space-4); }
  h2, h3, p { margin: 0; }
  h2 { color: var(--text-primary); font-size: var(--font-size-lg); }
  h3 { color: var(--text-primary); font-size: var(--font-size-md); }
  .eyebrow { color: var(--text-muted); font-size: var(--font-size-2xs); font-weight: 600; letter-spacing: .04em; text-transform: uppercase; }
  .result-count { color: var(--text-muted); font-size: var(--font-size-xs); white-space: nowrap; }
  .state { color: var(--text-secondary); font-size: var(--font-size-sm); }
  .state--error { color: var(--text-danger); }
  .action-filters { display: grid; min-width: 0; grid-template-columns: minmax(0, 12rem) minmax(0, 1fr) auto; gap: var(--space-3); align-items: end; }
  .filter-field { display: grid; min-width: 0; gap: var(--space-1); color: var(--text-muted); font-size: var(--font-size-xs); }
  @media (max-width: 760px) {
    .panel-heading, .overview-section, .actions-section { padding-inline: var(--space-4); }
    .action-filters { grid-template-columns: 1fr; align-items: stretch; }
    .action-filters :global(.kit-select-dropdown) { width: 100%; }
  }
</style>
