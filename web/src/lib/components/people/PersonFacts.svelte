<script lang="ts">
  import { Button } from '@kenn-io/kit-ui';
  import { onDestroy, untrack } from 'svelte';
  import type { APIClient } from '../../api/client';
  import type { DirectoryProfileController } from '../../directory/profile-controller.svelte';
  import type { DirectoryEntityController } from '../../directory/entity-controller.svelte';
  import { FactLedgerController } from '../../directory/fact-ledger-controller.svelte';
  import { formatShortDate } from '../../util/format';
  import FactLedger from '../directory/FactLedger.svelte';
  import type { FactSummary } from '../../api/generated/models';
  import DetailSection from './DetailSection.svelte';

  let { client, personID, refreshKey, profile, entity, onSetUp, onManageFields }: {
    client: APIClient; personID: number; refreshKey?: unknown;
    profile?: DirectoryProfileController | null; entity?: DirectoryEntityController | null;
    onSetUp?: () => void; onManageFields?: () => void;
  } = $props();
  const controller = new FactLedgerController(untrack(() => client));
  let open = $state(false);
  let stored = $state<{ personID: number; data: FactSummary } | null>(null);

  $effect(() => {
    const id = personID; refreshKey;
    stored = null;
    const abort = new AbortController();
    void controller.readSummary(id, abort.signal).then(data => {
      if (!abort.signal.aborted) stored = { personID: id, data };
    }).catch(() => {});
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
    const id = personID, shown = open; refreshKey;
    untrack(() => {
      if (shown && controller.active && controller.personID === id) void controller.retrySection(controller.selectedSection);
      else controller.applyContext(shown, id);
    });
  });
  onDestroy(() => controller.destroy());
</script>

{#snippet controls()}
  <span class="section-controls">
    {#if onSetUp}<Button size="sm" label="Set up" ariaLabel="Set up Facts" onclick={onSetUp} />{/if}
    <Button size="sm" label={open ? 'Hide' : 'Show'} ariaExpanded={open} onclick={() => (open = !open)} />
  </span>
{/snippet}

<DetailSection title="Facts" summary={!open ? summary : undefined} expanded={open} control={controls}>
  <FactLedger {controller} {profile} {entity} {onManageFields} />
</DetailSection>

<style>
  .section-controls { display: flex; align-items: center; gap: var(--control-gap); }
</style>
