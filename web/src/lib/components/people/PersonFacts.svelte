<script lang="ts">
  import { onDestroy, untrack } from 'svelte';
  import type { APIClient } from '../../api/client';
  import type { DirectoryProfileController } from '../../directory/profile-controller.svelte';
  import type { DirectoryEntityController } from '../../directory/entity-controller.svelte';
  import { FactLedgerController } from '../../directory/fact-ledger-controller.svelte';
  import { formatShortDate } from '../../util/format';
  import FactLedger from '../directory/FactLedger.svelte';
  import DetailSection from './DetailSection.svelte';
  import EmptyLine from '../common/EmptyLine.svelte';
  import InlineNotice from '../common/InlineNotice.svelte';
  import type { FactSummary } from '../../api/generated/models';

  let { client, personID, refreshKey, profile, entity, onSetUp, onManageFields }: {
    client: APIClient; personID: number; refreshKey?: unknown;
    profile?: DirectoryProfileController | null; entity?: DirectoryEntityController | null;
    onSetUp?: () => void; onManageFields?: () => void;
  } = $props();
  const controller = new FactLedgerController(untrack(() => client));
  let open = $state(false);
  let stored = $state<{ personID: number; data: FactSummary } | null>(null);
  let summaryError = $state(false);
  let retry = $state(0);
  const openable = $derived(!onSetUp || summaryError || stored?.personID !== personID || stored.data.has_stored_facts);
  $effect(() => {
    const id = personID; refreshKey; entity?.employments; profile?.attributes; retry;
    stored = null; summaryError = false;
    const abort = new AbortController();
    void controller.readSummary(id, abort.signal).then(data => {
      if (!abort.signal.aborted) stored = { personID: id, data };
    }).catch(() => { if (!abort.signal.aborted) summaryError = true; });
    return () => abort.abort();
  });
  const summary = $derived.by(() => {
    const data = stored?.personID === personID ? stored.data : null;
    if (!data) return undefined;
    if (!data.claimed_fields) return 'No claims yet';
    const count = `${data.claimed_fields} ${data.claimed_fields === 1 ? 'field' : 'fields'}`;
    return data.latest_claim_at ? `${count} · last found ${formatShortDate(data.latest_claim_at)}` : count;
  });
  $effect(() => {
    const id = personID, shown = open && openable; refreshKey; entity?.employments;
    untrack(() => {
      if (shown && controller.active && controller.personID === id) {
        void controller.retrySection('pins');
        if (controller.selectedSection !== 'pins') void controller.retrySection(controller.selectedSection);
      } else controller.applyContext(shown, id);
    });
  });
  onDestroy(() => controller.destroy());
</script>

{#snippet setUpLine()}<EmptyLine message="People lookups are off." onsetup={onSetUp} setupLabel="Set up Facts" />{/snippet}
{#snippet factsNotice()}
  {#if onSetUp}{@render setUpLine()}{/if}
  {#if summaryError}<InlineNotice message="Couldn't load facts summary." actionLabel="Retry" onaction={() => retry++} />{/if}
{/snippet}
{#if !openable}
  <DetailSection title="Facts">{@render setUpLine()}</DetailSection>
{:else}
  <DetailSection title="Facts" collapsible bind:open summary={!open ? summary : undefined} notice={onSetUp || summaryError ? factsNotice : undefined}>
    <FactLedger {controller} {profile} {entity} {onManageFields} />
  </DetailSection>
{/if}
