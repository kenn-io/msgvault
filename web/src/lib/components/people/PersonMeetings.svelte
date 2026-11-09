<script lang="ts">
  import { onDestroy, untrack } from 'svelte';
  import type { APIClient } from '../../api/client';
  import type { MeetingRef } from '../../api/generated/models';
  import { createMeetingsAPI } from '../../meetings/api';
  import { MeetingsController } from '../../meetings/controller.svelte';
  import MeetingPanel from '../meetings/MeetingPanel.svelte';
  import EmptyLine from '../common/EmptyLine.svelte';
  import DetailSection from './DetailSection.svelte';

  let { client, personID, refreshKey = '', onOpenMeeting }:
    { client: APIClient; personID: number; refreshKey?: string; onOpenMeeting?: (meeting: MeetingRef) => void } = $props();
  const controller = new MeetingsController(createMeetingsAPI(untrack(() => client)));
  let settledEmpty = $state<{ personID: number; empty: boolean }>();
  $effect.pre(() => {
    const scope = { kind: 'direct' as const, scope: { person_id: personID } }, key = refreshKey;
    untrack(() => { void controller.setScope(scope, key); });
  });
  $effect(() => {
    if (controller.metricsLoading || controller.actionsLoading || !controller.metrics || !controller.actions) return;
    settledEmpty = { personID, empty: controller.metrics.totals.meeting_count === 0 && controller.actions.total_count === 0 };
  });
  onDestroy(() => controller.destroy());
  // With no meetings and no follow-ups, the panel would be a dashboard of zeros; one line says it.
  const empty = $derived(settledEmpty?.personID === personID && settledEmpty.empty && !controller.actionsError && !controller.metricsError);
</script>

{#if settledEmpty?.personID !== personID && !controller.metricsError && !controller.actionsError}
  <DetailSection title="Meetings"><EmptyLine message="Loading meetings…" /></DetailSection>
{:else if empty}
  <DetailSection title="Meetings"><EmptyLine message="No meetings yet" /></DetailSection>
{:else}
  <MeetingPanel {client} {controller} scope={{ kind: 'direct', scope: { person_id: personID } }} {refreshKey} {onOpenMeeting} />
{/if}
