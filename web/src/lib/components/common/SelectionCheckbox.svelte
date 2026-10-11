<script lang="ts">
  import { Checkbox } from '@kenn-io/kit-ui';

  interface Props {
    checked: boolean;
    label: string;
    mixed?: boolean;
    disabled?: boolean;
    /** Inside a grid, the grid keeps focus and the only Tab stop, and its own keys select rows. */
    inGrid?: boolean;
    onToggle: (range: boolean) => void;
  }

  let {
    checked,
    label,
    mixed = false,
    disabled = false,
    inGrid = false,
    onToggle
  }: Props = $props();

  let range = false;
  let wrapper: HTMLSpanElement;

  function toggle(): void {
    onToggle(range);
    // A click focuses the checkbox, where the grid's keys no longer reach.
    if (inGrid) wrapper.closest<HTMLElement>('[role="grid"]')?.focus();
  }
</script>

<!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_static_element_interactions --
     The nested Kit checkbox owns keyboard semantics; this wrapper only keeps
     Shift and pointer events from reaching the selectable row beneath it. -->
<span
  bind:this={wrapper}
  class="selection-checkbox"
  {@attach (wrapper) => {
    // Kit's Checkbox has no tabindex prop.
    if (inGrid) wrapper.querySelector('input')?.setAttribute('tabindex', '-1');
  }}
  onpointerdown={(event) => {
    range = event.shiftKey;
    event.stopPropagation();
  }}
  onclick={(event) => {
    range = event.shiftKey;
    event.stopPropagation();
  }}
>
  <Checkbox
    {checked}
    indeterminate={mixed}
    {disabled}
    ariaLabel={label}
    onchange={toggle}
  />
</span>

<style>
  .selection-checkbox {
    display: grid;
    width: 16px;
    height: 16px;
    flex: none;
    place-items: center;
  }

  .selection-checkbox :global(.kit-checkbox) {
    width: 16px;
    height: 16px;
  }
</style>
