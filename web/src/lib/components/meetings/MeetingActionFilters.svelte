<script lang="ts">
  import { onDestroy, untrack } from 'svelte';
  import { Button, SelectDropdown, TextInput } from '@kenn-io/kit-ui';
  import type { MeetingActionsRequest } from '../../api/generated/models';
  import type { MeetingsController } from '../../meetings/controller.svelte';

  let { controller, compact = false }: { controller: MeetingsController; compact?: boolean } = $props();
  let status = $state(untrack(() => controller.filters.status ?? ''));
  let assignee = $state(untrack(() => controller.filters.assigneeEmail ?? ''));
  let assigneeTimer: ReturnType<typeof setTimeout> | undefined;
  const statuses = $derived([{ value: '', label: compact ? 'All statuses' : 'All source statuses' },
    { value: 'pending', label: 'Pending' }, { value: 'completed', label: 'Completed' },
    { value: 'cancelled', label: 'Cancelled' }, { value: 'unknown', label: 'Unknown' }]);

  function apply(): void {
    clearTimeout(assigneeTimer);
    void controller.setFilters({ status: (status || undefined) as MeetingActionsRequest['status'], assigneeEmail: assignee });
  }
  onDestroy(() => clearTimeout(assigneeTimer));
</script>

{#snippet fields()}
  <div class="filter-field">
    {#if !compact}<span>Status</span>{/if}
    <SelectDropdown title={compact ? 'Status' : 'Source status'} value={status} options={statuses} onchange={(value) => { status = value; if (compact) apply(); }} />
  </div>
  <div class="filter-field">
    {#if !compact}<span>Assignee</span>{/if}
    <TextInput value={assignee} ariaLabel={compact ? 'Assignee' : 'Assignee email'} placeholder={compact ? 'Assignee email' : 'Email address'} block oninput={(value) => {
      assignee = value;
      if (compact) { clearTimeout(assigneeTimer); assigneeTimer = setTimeout(apply, 300); }
    }} />
  </div>
{/snippet}

{#if compact}
  <div class="action-filters compact" aria-label="Action item filters">{@render fields()}</div>
{:else}
  <form class="action-filters" aria-label="Action item filters" onsubmit={(event) => { event.preventDefault(); apply(); }}>
    {@render fields()}
    <Button type="submit" label="Apply action filters" size="sm" surface="outline" />
  </form>
{/if}

<style>
  .action-filters { display: grid; min-width: 0; grid-template-columns: minmax(0, 12rem) minmax(0, 1fr) auto; gap: var(--space-3); align-items: end; }
  .filter-field { display: grid; min-width: 0; gap: var(--space-1); color: var(--text-muted); font-size: var(--font-size-xs); }
  .compact { grid-template-columns: 1fr 1fr; gap: var(--space-2); align-items: stretch; }
  @media (max-width: 760px) {
    .action-filters:not(.compact) { grid-template-columns: 1fr; align-items: stretch; }
    .action-filters:not(.compact) :global(.kit-select-dropdown) { width: 100%; }
  }
</style>
