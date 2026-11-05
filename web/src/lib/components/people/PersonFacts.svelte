<script lang="ts">
  import { Button, Menu, MenuContent, MenuItem, MenuTrigger } from '@kenn-io/kit-ui';
  import Ellipsis from '@lucide/svelte/icons/ellipsis';
  import { getContext, onDestroy, tick, untrack } from 'svelte';

  import type { APIClient } from '../../api/client';
  import { FactLedgerController } from '../../directory/fact-ledger-controller.svelte';
  import FactLedger from '../directory/FactLedger.svelte';

  import { personDirectoryContext, type PersonDirectoryGetter } from '../../directory/person-directory-context';

  interface Props {
    client: APIClient;
    personID: number;
    refreshKey?: unknown;
    /** Opens the setting that turns fact review on, while it is off. Stored facts stay readable. */
    onSetUp?: () => void;
  }

  let { client, personID, refreshKey, onSetUp = undefined }: Props = $props();

  const getDirectory = getContext<PersonDirectoryGetter | undefined>(personDirectoryContext);
  const directory = $derived(getDirectory?.());
  const profile = $derived(directory?.profile);
  const entity = $derived(directory?.entity);
  const controller = new FactLedgerController(untrack(() => client));
  let open = $state(false);

  // Nothing loads until the section opens.
  $effect(() => {
    const id = personID, shown = open; refreshKey; entity?.employments;
    untrack(() => {
      if (shown && controller.active && controller.personID === id) {
        void controller.retrySection('pins');
        if (controller.selectedSection !== 'pins') void controller.retrySection(controller.selectedSection);
      } else controller.applyContext(shown, id);
    });
  });

  onDestroy(() => controller.destroy());
</script>

<section aria-labelledby={`facts-${personID}`}>
  <header>
    <h3 id={`facts-${personID}`}>Facts</h3>
    <span class="spacer"></span>
    {#if onSetUp}<Menu align="end"><MenuTrigger ariaLabel="Facts actions"><Ellipsis size={16} aria-hidden="true" /></MenuTrigger><MenuContent ariaLabel="Facts actions"><MenuItem textValue="Set up" onselect={async () => { await tick(); onSetUp?.(); }}>Set up</MenuItem></MenuContent></Menu>{/if}
    <Button size="sm" label={open ? 'Hide' : 'Show'} ariaExpanded={open} onclick={() => (open = !open)} />
  </header>
  {#if open}<FactLedger {controller} {profile} {entity} />{/if}
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
