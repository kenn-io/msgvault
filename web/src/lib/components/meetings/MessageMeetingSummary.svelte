<script lang="ts">
  import { Button } from '@kenn-io/kit-ui';
  import { onDestroy, untrack } from 'svelte';
  import type { APIClient } from '../../api/client';
  import type { MeetingRef } from '../../api/generated/models';
  import { canonicalFingerprint } from '../../explore/selection';
  import { formatMeetingDuration } from '../../meetings/format';
  import { createMeetingsAPI } from '../../meetings/api';
  import { MeetingsController, type MeetingPanelScope } from '../../meetings/controller.svelte';
  import MeetingActionsDialog from './MeetingActionsDialog.svelte';

  let { client, scope, refreshKey = '', onReloadScope, onOpenMeeting }: {
    client: APIClient; scope: MeetingPanelScope; refreshKey?: string; onReloadScope?: () => void; onOpenMeeting?: (meeting: MeetingRef) => void;
  } = $props();
  const controller = new MeetingsController(createMeetingsAPI(untrack(() => client)));
  const fingerprint = $derived(canonicalFingerprint({ scope, refreshKey }));
  let open = $state(false);
  $effect.pre(() => { void fingerprint; untrack(() => { open = false; void controller.setScope(scope, refreshKey); }); });
  onDestroy(() => controller.destroy());
  function reload(): void {
    if (scope.kind === 'explore' && onReloadScope) onReloadScope();
    else void controller.reload();
  }
</script>

<section class="meeting-summary" aria-label="Meeting summary">
  {#if controller.metricsLoading}<span role="status">Loading meeting summary…</span>{/if}
  {#if controller.metricsError}
    <span role="alert">{controller.metricsError.message}</span>
    {#if controller.metricsError.recovery === 'reload' || controller.metricsError.recovery === 'retry'}<Button label="Reload meeting activity" onclick={reload} />{/if}
  {/if}
  {#if controller.metrics}
    <span>{controller.metrics.totals.meeting_count} meetings · {formatMeetingDuration(controller.metrics.totals.total_known_seconds)} known</span>
    {#if controller.metrics.totals.unknown_duration_count}<span>· {controller.metrics.totals.unknown_duration_count} unknown duration</span>{/if}
    <Button size="sm" surface="soft" label={controller.actions ? `Action items · ${controller.actions.total_count}` : 'Action items'} onclick={() => open = true} />
  {/if}
</section>
{#if open}<MeetingActionsDialog {client} {controller} onclose={() => open = false} onReload={reload} {onOpenMeeting} />{/if}

<style>
  .meeting-summary { display: flex; flex-wrap: wrap; align-items: center; gap: var(--space-2); color: var(--text-secondary); font-size: var(--font-size-sm); }
</style>
