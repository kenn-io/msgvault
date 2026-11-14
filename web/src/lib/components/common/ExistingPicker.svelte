<script lang="ts">
  import { Button } from '@kenn-io/kit-ui';

  interface Item { id: string; label: string; detail?: string }
  interface Props {
    /** The choices, each a name over its Meta detail. Choosing one advances the flow. */
    items: Item[];
    /** Names the list for screen readers. */
    legend: string;
    onadvance: (item: Item) => void;
  }

  let { items, legend, onadvance }: Props = $props();
</script>

<ul class="results" aria-label={legend}>
  {#each items as item (item.id)}
    <li><Button class="advance-option" ariaLabel={item.detail ? `${item.label} ${item.detail}` : item.label} onclick={() => onadvance(item)}>
      <span class="option"><span>{item.label}</span>{#if item.detail}<small>{item.detail}</small>{/if}</span>
    </Button></li>
  {/each}
</ul>

<style>
  /* List items at the body's full width, with no box of its own: 32 for a name, 54 for a name over its
   * Meta detail. The list pulls out by its items' padding, so a name starts on the body's line and the
   * hover fill reaches 4 from the dialog's edge; on the right it stops short of the bar's gutter. */
  .results {
    --list-item-pad-x: var(--space-group);
    display: grid;
    min-width: 0;
    margin: 0 calc(-1 * min(var(--list-item-pad-x), var(--pane-pad-x) - var(--scrollbar-gutter-size))) 0 calc(-1 * var(--list-item-pad-x));
    padding: 0;
    list-style: none;
  }
  /* An advance row is a list item, not a button: no fill until hover, like a menu item. */
  .results :global(.kit-button.advance-option) { box-sizing: border-box; width: 100%; min-height: var(--list-item-h); padding: 0 var(--list-item-pad-x); justify-content: flex-start; border: 0; background: transparent; box-shadow: none; }
  .results :global(.kit-button.advance-option:hover) { background: var(--bg-surface-hover); }
  .results :global(.kit-button.advance-option:has(small)) { min-height: var(--list-item-h-2); padding-block: var(--space-group); }
  .option { display: grid; min-width: 0; text-align: left; white-space: normal; overflow-wrap: anywhere; font-size: var(--type-body-size); font-weight: 400; line-height: var(--type-body-line); }
  small { color: var(--text-muted); font-size: var(--type-meta-size); line-height: var(--type-meta-line); }
</style>
