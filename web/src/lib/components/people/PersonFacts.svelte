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

<section aria-labelledby={`facts-${personID}`}>
  <header>
    <h3 id={`facts-${personID}`}>Facts</h3>
    {#if !open && summary}<span class="summary">{summary}</span>{/if}
    <span class="spacer"></span>
    {#if onSetUp}<Button size="sm" label="Set up" ariaLabel="Set up Facts" onclick={onSetUp} />{/if}
    <Button size="sm" label={open ? 'Hide' : 'Show'} ariaExpanded={open} onclick={() => (open = !open)} />
  </header>
  {#if open}<FactLedger {controller} {profile} {entity} {onManageFields} />{/if}
</section>

<style>
  section {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }

  header {
    display: flex;
    min-height: var(--control-h);
    align-items: center;
    gap: var(--control-gap);
  }

  .spacer { flex: 1; }
  .summary { color: var(--text-muted); font-size: var(--font-size-sm); }

  h3 {
    margin: 0;
    color: var(--text-primary);
    font-size: var(--font-size-md);
    font-weight: var(--font-weight-semibold, 600);
  }
</style>
