<script lang="ts">
  import type { Snippet } from 'svelte';

  interface Props {
    label?: string;
    /** A muted second line under the value, such as dates or a role. */
    description?: string;
    children: Snippet;
    /** Buttons for the row, always shown at its right. */
    actions?: Snippet;
  }

  let { label = undefined, description = undefined, children, actions = undefined }: Props = $props();
</script>

<div class="detail-row">
  {#if label !== undefined}<span class="label">{label}</span>{/if}
  <div class="value">{@render children()}{#if description}<span class="description">{description}</span>{/if}</div>
  {#if actions}<div class="actions">{@render actions()}</div>{/if}
</div>

<style>
  /* 8 above and below, so one line (Body) is 36 and two lines (Body, then Meta) are 54. The divider is
   * drawn inside the bottom padding, so it adds no height. */
  .detail-row {
    display: flex;
    align-items: baseline;
    gap: var(--space-group);
    padding: var(--space-group) 0;
    box-shadow: inset 0 -1px 0 var(--border-muted);
  }

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
    display: flex;
    flex: none;
    align-self: flex-start;
    align-items: center;
    gap: var(--space-group);
    margin: calc((var(--type-body-line) - var(--control-h)) / 2) 0;
    margin-left: auto;
  }

  /* On a phone the value takes the full width and the actions wrap under it. */
  @media (max-width: 640px) {
    .detail-row:has(.actions) { flex-wrap: wrap; }
    .detail-row:has(.actions) .value { flex-basis: 100%; }
    .actions { flex-wrap: wrap; max-width: 100%; }
  }
</style>
