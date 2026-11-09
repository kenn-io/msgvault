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
  $effect.pre(() => {
    const scope = { kind: 'direct' as const, scope: { person_id: personID } }, key = refreshKey;
    untrack(() => { void controller.setScope(scope, key); });
  });
  onDestroy(() => controller.destroy());
  // With no meetings and no follow-ups, the panel would be a dashboard of zeros; one line says it.
  const empty = $derived(controller.metrics?.totals.meeting_count === 0 && controller.actions?.total_count === 0 && !controller.actionsError && !controller.metricsError && !controller.metricsLoading && !controller.actionsLoading);
</script>

{#if empty}
  <DetailSection title="Meetings"><EmptyLine message="No meetings yet" /></DetailSection>
{:else}
  <MeetingPanel {client} {controller} scope={{ kind: 'direct', scope: { person_id: personID } }} {refreshKey} {onOpenMeeting} />
{/if}
