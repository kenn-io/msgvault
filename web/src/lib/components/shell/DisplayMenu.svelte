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
</script>

<Menu align="end">
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
