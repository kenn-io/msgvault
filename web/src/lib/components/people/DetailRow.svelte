<script lang="ts">
  import { Button, Menu, MenuTrigger, MenuContent, MenuItem } from '@kenn-io/kit-ui';
  import type { Snippet } from 'svelte';
  import type { ProvenanceLine } from '../directory/history-format';

  interface Props {
    label?: string;
    /** The row's actions: one shows as its button, more go in a ⋯ menu named for the row. */
    menu?: { label: string; ariaLabel?: string; onselect: () => void; danger?: boolean; disabled?: boolean }[];
    exception?: string;
    /** A muted second line under the value: dates or a role, or a provenance line with its timestamps as the tooltip. */
    description?: string | ProvenanceLine;
    children: Snippet;
    /** Buttons for the row, always shown at its right. */
    actions?: Snippet;
    /** Makes the whole row one button that opens what it names. */
    onclick?: () => void;
    actionLabel?: string;
  }

  let { label = undefined, description = undefined, children, actions = undefined, onclick = undefined, actionLabel = 'Open', menu = [], exception = undefined }: Props = $props();
  const line = $derived(typeof description === 'string' ? { text: description, title: undefined } : description);
</script>

<div class="detail-row" class:own-actions={menu.length > 0} class:clickable={!!onclick}>
  {#if onclick}<button class="row-hit" type="button" aria-label={actionLabel} title={line?.title} {onclick}></button>{/if}
  {#if label !== undefined}<span class="label">{label}</span>{/if}
  <div class="value">{@render children()}{#if line?.text || exception}<span class="description" title={line?.title}>{line?.text}{#if line?.text && exception}{' · '}{/if}{#if exception}<span class="exception">{exception}</span>{/if}</span>{/if}</div>
  {#if actions || menu.length}<div class="actions">{@render actions?.()}
    {#if menu.length === 1}<Button label={menu[0].label} ariaLabel={menu[0].ariaLabel} tone={menu[0].danger ? 'danger' : 'neutral'} disabled={menu[0].disabled} onclick={menu[0].onselect} />
    {:else if menu.length}<Menu align="end"><MenuTrigger ariaLabel={`Actions for ${actionLabel}`}>⋯</MenuTrigger><MenuContent ariaLabel={`Actions for ${actionLabel}`}>
      {#each menu as item}<MenuItem onselect={item.onselect} disabled={item.disabled} tone={item.danger ? 'danger' : 'neutral'}>{item.label}</MenuItem>{/each}
    </MenuContent></Menu>{/if}
  </div>{/if}
</div>

<style>
  /* 8 above and below, so one line (Body) is 36 and two lines (Body, then Meta) are 54. The divider is
   * drawn inside the bottom padding, so it adds no height. */
  .detail-row {
    position: relative;
    display: flex;
    align-items: baseline;
    gap: var(--space-group);
    padding: var(--row-pad-y) var(--row-pad-x);
    box-shadow: inset 0 -1px 0 var(--border-muted);
  }

  /* A clickable row is one button under its text, filled on hover; links and actions in it stay their own. */
  .row-hit {
    position: absolute;
    inset: 0;
    width: 100%;
    padding: 0;
    border: 0;
    border-radius: var(--radius-sm);
    background: transparent;
    cursor: pointer;
  }
  .row-hit:focus-visible { outline: var(--focus-ring); outline-offset: -2px; }
  .clickable:hover { background: var(--bg-surface-hover); }
  .clickable .label, .clickable .value { pointer-events: none; }
  .clickable .value :global(:is(a, button)) { position: relative; pointer-events: auto; }

  /* Label | value: one 160 label column (120 on a phone), 8, then the value, on a shared first baseline. */
  .label {
    width: var(--detail-label-width);
    flex: none;
    color: var(--text-muted);
    font-size: var(--type-meta-size);
    line-height: var(--type-meta-line);
  }

  .value {
    min-width: 0;
    flex: 1;
    overflow-wrap: anywhere;
    color: var(--text-primary);
    font-size: var(--type-body-size);
    line-height: var(--type-body-line);
  }

  /* A value that folds its detail under a plain Disclosure keeps the first line of an ordinary value:
   * no padding above, Body on its 20 line, and the title's baseline as the row's. The chevron centers
   * on that line, and the actions stay on it whether Details is open or not. */
  .detail-row .value :global(.kit-disclosure--plain > .kit-disclosure__summary) {
    align-items: baseline;
    padding-block: 0;
    font-size: var(--type-body-size);
    line-height: var(--type-body-line);
  }

  .detail-row .value :global(.kit-disclosure--plain > .kit-disclosure__summary > .kit-disclosure__chevron) {
    align-self: center;
  }

  .description {
    display: block;
    color: var(--text-muted);
    font-size: var(--type-meta-size);
    line-height: var(--type-meta-line);
  }

  /* Actions sit at the right, 8 apart, centered on the first line's 20: a 28 control starts 4 above the
   * line and ends 4 below it, so it never adds height to the row. */
  .actions {
    position: relative;
    display: flex;
    flex: none;
    align-self: flex-start;
    align-items: center;
    gap: var(--space-group);
    margin: calc((var(--type-body-line) - var(--control-h)) / 2) 0;
    margin-left: auto;
  }

  .actions :global(.kit-menu__trigger) { width: var(--control-h); min-height: var(--control-h); padding: 0; }

  /* On a phone the value takes the full width and the actions wrap under it; a row's own actions stay on its first line. */
  @media (max-width: 640px) {
    .detail-row:not(.own-actions):has(.actions) { flex-wrap: wrap; }
    .detail-row:not(.own-actions):has(.actions) .value { flex-basis: 100%; }
    .actions { flex-wrap: wrap; max-width: 100%; }
  }
</style>
