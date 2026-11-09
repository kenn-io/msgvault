<script lang="ts" module>
  /** One profile in a merge or split: its name, then the addresses it holds. */
  export interface MergeCard {
    title: string;
    addresses: { id: string; value: string; detail?: string }[];
    /** The line for a card with no addresses. */
    empty?: string;
  }
</script>

<script lang="ts">
  import ArrowRight from '@lucide/svelte/icons/arrow-right';
  import { addressText } from '../../directory/merge-labels';
  let { from, to, label }: { from: MergeCard; to: MergeCard; label: string } = $props();
</script>

{#snippet card(profile: MergeCard)}
  <!-- A heading that reads as an address stands for one of them; another that reads the same still lists. -->
  {@const heading = profile.addresses.findIndex(address => addressText({ title: address.value, detail: address.detail }) === profile.title)}
  {@const addresses = profile.addresses.filter((_, index) => index !== heading)}
  <section class="card" aria-label={profile.title}>
    <h3>{profile.title}</h3>
    {#if addresses.length}
      <ul>
        {#each addresses as address (address.id)}
          <li>{address.value}{#if address.detail}<small>{address.detail}</small>{/if}</li>
        {/each}
      </ul>
    {:else if !profile.addresses.length}<p>{profile.empty ?? 'No addresses'}</p>{/if}
  </section>
{/snippet}

<div class="cards" role="group" aria-label={label}>
  {@render card(from)}
  <span class="arrow" aria-hidden="true"><ArrowRight size={16} /></span>
  {@render card(to)}
</div>

<style>
  /* Two cards padded 12, 12 apart with the arrow centered between them; they stack on a phone. Inside a
   * card, the name (Heading) is 8 above its addresses, each a Body line with its Meta detail under it. */
  .cards { display: grid; grid-template-columns: minmax(0, 1fr) auto minmax(0, 1fr); align-items: stretch; gap: var(--space-box); }
  .card { display: grid; align-content: start; gap: var(--space-group); min-width: 0; padding: var(--space-box); border: 1px solid var(--border-muted); border-radius: var(--radius-sm); }
  .arrow { display: flex; align-items: center; justify-content: center; color: var(--text-muted); }
  h3, ul, p { margin: 0; }
  h3 { font-size: var(--type-heading-size); line-height: var(--type-heading-line); overflow-wrap: anywhere; }
  ul { display: grid; gap: var(--space-tight); padding: 0; list-style: none; }
  li { display: grid; min-width: 0; overflow-wrap: anywhere; font-size: var(--type-body-size); line-height: var(--type-body-line); }
  small, p { color: var(--text-muted); font-size: var(--type-meta-size); line-height: var(--type-meta-line); }
  @media (max-width: 640px) {
    .cards { grid-template-columns: minmax(0, 1fr); }
    .arrow :global(svg) { transform: rotate(90deg); }
  }
</style>
