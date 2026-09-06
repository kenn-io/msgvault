<script lang="ts">
  import { Button, EmptyState } from '@kenn-io/kit-ui';

  import type { FactLedgerController } from '../../directory/fact-ledger-controller.svelte';
  import FactLedger from './FactLedger.svelte';

  interface Props {
    controller: FactLedgerController;
    personID: number | null;
    onOpenDirectory?: () => void;
    onOpenPerson?: (personID: number) => void;
  }
  let { controller, personID, onOpenDirectory = () => undefined, onOpenPerson = () => undefined }: Props = $props();
</script>

<section class="fact-review" aria-labelledby="fact-review-heading">
  <h2 id="fact-review-heading" tabindex="-1">Fact review</h2>
  {#if personID === null}
    <div class="chooser">
      <EmptyState
        title="Choose a person in Directory to inspect their fact ledger"
        description="Fact diagnostics are scoped to one durable Directory person."
      />
      <Button label="Open Directory" onclick={onOpenDirectory} />
    </div>
  {:else}
    <div class="person-context">
      <strong>Person ID {personID}</strong>
      <Button label="Open person profile" size="sm" onclick={() => onOpenPerson(personID)} />
    </div>
    <div class="notices" aria-label="Unavailable fact features">
      <p>Fact candidate decisions are unavailable until a generated candidate contract is installed.</p>
      <p>A dated last-time-we-talked brief is unavailable until the server exposes a generated brief contract.</p>
    </div>
    <FactLedger {controller} />
  {/if}
</section>

<style>
  .fact-review { display: grid; gap: var(--space-4); }
  h2 { margin: 0; }
  .chooser { display: grid; justify-items: center; gap: var(--space-3); }
  .person-context { display: flex; align-items: center; justify-content: space-between; gap: var(--space-3); flex-wrap: wrap; }
  .notices { display: grid; gap: var(--space-2); padding: var(--space-3); border-left: 2px solid var(--border-strong); color: var(--text-secondary); }
  .notices p { margin: 0; }
</style>
