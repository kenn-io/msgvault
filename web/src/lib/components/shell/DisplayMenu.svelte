<script lang="ts">
  import {
    Menu,
    MenuContent,
    MenuItem,
    MenuRadioGroup,
    MenuRadioItem,
    MenuSeparator,
    MenuTrigger,
  } from '@kenn-io/kit-ui';
  import SlidersHorizontal from '@lucide/svelte/icons/sliders-horizontal';

  type Density = 'daemon' | 'compact' | 'comfortable';

  interface Props {
    density: Density;
    themeOverridden: boolean;
    onDensityChange: (value: Density) => void;
    onUseDaemonTheme: () => void;
  }

  let { density, themeOverridden, onDensityChange, onUseDaemonTheme }: Props = $props();
  let open = $state(false);
  let root = $state<HTMLElement>();

  // Kit closes the menu on Escape but lets the key reach the window shortcuts, which would also
  // close the reading pane or remove a grouping. The open menu keeps Escape to itself.
  function closeOnEscape(event: KeyboardEvent): void {
    if (event.key !== 'Escape' || !open) return;
    event.stopPropagation();
    open = false;
    root?.querySelector<HTMLElement>('[aria-haspopup]')?.focus();
  }
</script>

<span class="display-menu" role="none" bind:this={root} onkeydown={closeOnEscape}>
  <Menu align="end" bind:open>
    <MenuTrigger ariaLabel="Display" title="Display">
      <SlidersHorizontal size={18} aria-hidden="true" />
    </MenuTrigger>
    <MenuContent ariaLabel="Display">
      <MenuRadioGroup
        ariaLabel="Temporary density"
        value={density}
        onchange={(value) => onDensityChange(value as Density)}
      >
        <MenuRadioItem value="daemon">Auto</MenuRadioItem>
        <MenuRadioItem value="compact">Compact</MenuRadioItem>
        <MenuRadioItem value="comfortable">Comfortable</MenuRadioItem>
      </MenuRadioGroup>
      {#if themeOverridden}
        <MenuSeparator />
        <MenuItem onselect={onUseDaemonTheme}>Use daemon theme</MenuItem>
      {/if}
    </MenuContent>
  </Menu>
</span>

<style>
  .display-menu {
    display: contents;
  }
</style>
