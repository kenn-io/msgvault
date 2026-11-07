<script lang="ts">
  import { Button } from '@kenn-io/kit-ui';
  import { onDestroy, untrack } from 'svelte';

  import type { DirectoryProfileController } from '../../directory/profile-controller.svelte';
  import type { APIClient } from '../../api/client';
  import { FactLedgerController } from '../../directory/fact-ledger-controller.svelte';
  import FactLedger from '../directory/FactLedger.svelte';

  interface Props {
    client: APIClient;
    personID: number;
    refreshKey?: unknown;
    profileController?: DirectoryProfileController;
    /** Opens the setting that turns fact review on, while it is off. Stored facts stay readable. */
    onSetUp?: () => void;
  }

  let { client, personID, refreshKey, profileController = undefined, onSetUp = undefined }: Props = $props();

  const controller = new FactLedgerController(untrack(() => client));
  let open = $state(false);

  // Nothing loads until the section opens.
  $effect(() => {
    const id = personID, shown = open; refreshKey;
    untrack(() => { if (shown && controller.active && controller.personID === id) void controller.retrySection(controller.selectedSection); else controller.applyContext(shown, id); });
  });

  onDestroy(() => controller.destroy());
</script>

<section aria-labelledby={`facts-${personID}`}>
  <header>
    <h3 id={`facts-${personID}`}>Facts</h3>
    <span class="spacer"></span>
    {#if onSetUp}<Button size="sm" label="Set up" ariaLabel="Set up Facts" onclick={onSetUp} />{/if}
    <Button size="sm" label={open ? 'Hide' : 'Show'} ariaExpanded={open} onclick={() => (open = !open)} />
  </header>
  {#if open}<FactLedger {controller} {profileController} />{/if}
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

  .spacer {
    flex: 1;
  }

  h3 {
    margin: 0;
    color: var(--text-primary);
    font-size: var(--font-size-md);
    font-weight: var(--font-weight-semibold, 600);
  }
</style>
