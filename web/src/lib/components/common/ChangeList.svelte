<script module lang="ts">
  /** One field that differs: the saved value and the person's draft. */
  export interface Change { label: string; saved: string; yours: string }
</script>

<script lang="ts">
  import type { ProvenanceLine } from '../directory/history-format';

  let { changes, provenance, otherChanged = false }: {
    /** Only the fields on the form that differ, in the form's order. */
    changes: Change[];
    /** When the saved version was saved, and by whom when it wasn't the owner: "Saved at 8:34 AM", built by `conflictSaved`. */
    provenance?: ProvenanceLine;
    /** Fields that changed but aren't on this form; their rows go in the notice's Details. */
    otherChanged?: boolean;
  } = $props();
</script>

<div class="change-list">
  {#if changes.length}
    <!-- Saved and Yours head their columns once; each row is the field, then its two values, so a long value wraps inside its own column. -->
    <div class="table" role="table" aria-label="What changed">
      <div class="row head" role="row">
        <span class="corner" role="columnheader"><span class="kit-sr-only">Field</span></span>
        <span role="columnheader">Saved</span>
        <span role="columnheader">Yours</span>
      </div>
      {#each changes as change (change.label)}
        <div class="row" role="row">
          <span class="label" role="rowheader">{change.label}</span>
          <span role="cell">{change.saved || 'None'}</span>
          <span role="cell">{change.yours || 'None'}</span>
        </div>
      {/each}
    </div>
  {:else}
    <p>Nothing you're editing changed.</p>
  {/if}
  {#if otherChanged}<p>Other details also changed</p>{/if}
  {#if provenance}<p class="provenance" title={provenance.title}>{provenance.text}</p>{/if}
</div>

<style>
  /* L3 label | value rows: the 160 label column, 8, then Saved and Yours as equal value columns 12 apart.
   * The provenance line (Meta) follows 8 below. */
  .change-list { display: grid; gap: var(--space-group); min-width: 0; container-type: inline-size; }
  p { margin: 0; font-size: var(--type-meta-size); line-height: var(--type-meta-line); }
  .table { display: grid; grid-template-columns: calc(var(--detail-label-width) + var(--space-group)) repeat(2, minmax(0, 1fr)); }
  .row { display: grid; grid-column: 1 / -1; grid-template-columns: subgrid; align-items: baseline; min-width: 0; padding-block: var(--row-pad-y); overflow-wrap: anywhere; font-size: var(--type-body-size); line-height: var(--type-body-line); }
  .row > :nth-child(2) { padding-right: calc(var(--space-box) / 2); }
  .row > :nth-child(3) { padding-left: calc(var(--space-box) / 2); }
  .head { padding-block: 0; }
  .label { padding-right: var(--space-group); color: var(--text-secondary); font-size: var(--type-meta-size); line-height: var(--type-meta-line); }
  .head, .provenance { color: var(--text-muted); font-size: var(--type-small-size); line-height: var(--type-small-line); }
  .provenance { font-size: var(--type-meta-size); line-height: var(--type-meta-line); }
  /* Too narrow for three columns: the field heads its row, and Saved and Yours keep their columns under it. */
  @container (max-width: 24rem) {
    .table { grid-template-columns: repeat(2, minmax(0, 1fr)); }
    /* The field reads as the form's own label for it, so it isn't mistaken for a third value. */
    .row > .label { grid-column: 1 / -1; padding-right: 0; font-weight: var(--font-weight-semibold); }
    .corner { position: absolute; width: 1px; height: 1px; overflow: hidden; }
  }
</style>
