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
  import Check from '@lucide/svelte/icons/check';
  import SlidersHorizontal from '@lucide/svelte/icons/sliders-horizontal';

  import { EXPLORE_COLUMNS } from '../../explore/labels';
  import type { ExploreColumn } from '../../explore/models';
  import type { DensityPreference, ThemePreference } from '../../theme/preferences.svelte';

  type ReaderPosition = 'below' | 'right';

  interface Props {
    open?: boolean;
    overridden?: boolean;
    onUseDefaults?: () => void;
    theme: ThemePreference;
    density: DensityPreference;
    onThemeChange: (value: ThemePreference) => void;
    onDensityChange: (value: DensityPreference) => void;
    /** Shown only where the page has a table of messages. */
    columns?: ExploreColumn[];
    onColumnsChange?: (columns: ExploreColumn[]) => void;
    /** Shown only where the reader can sit beside the list. */
    readerPosition?: ReaderPosition;
    onReaderPositionChange?: (value: ReaderPosition) => void;
  }

  let {
    open = $bindable(false),
    overridden = false,
    onUseDefaults = () => undefined,
    theme,
    density,
    onThemeChange,
    onDensityChange,
    columns = undefined,
    onColumnsChange = undefined,
    readerPosition = undefined,
    onReaderPositionChange = undefined,
  }: Props = $props();

  function toggleColumn(column: ExploreColumn): void {
    if (!columns || !onColumnsChange) return;
    const shown = columns.includes(column);
    onColumnsChange(
      EXPLORE_COLUMNS.map(({ id }) => id).filter((id) => (id === column ? !shown : columns.includes(id))),
    );
  }
</script>

<Menu bind:open align="end">
  <MenuTrigger ariaLabel="Display" title="Display">
    <SlidersHorizontal size={16} aria-hidden="true" />
  </MenuTrigger>
  <MenuContent ariaLabel="Display" class="display-menu">
    <div class="display-menu__heading" aria-hidden="true">Theme</div>
    <MenuRadioGroup ariaLabel="Theme" value={theme} onchange={(value) => onThemeChange(value as ThemePreference)}>
      <MenuRadioItem value="system">System</MenuRadioItem>
      <MenuRadioItem value="light">Light</MenuRadioItem>
      <MenuRadioItem value="dark">Dark</MenuRadioItem>
    </MenuRadioGroup>
    <MenuSeparator />
    <div class="display-menu__heading" aria-hidden="true">Density</div>
    <MenuRadioGroup
      ariaLabel="Density"
      value={density}
      onchange={(value) => onDensityChange(value as DensityPreference)}
    >
      <MenuRadioItem value="compact">Compact</MenuRadioItem>
      <MenuRadioItem value="comfortable">Comfortable</MenuRadioItem>
    </MenuRadioGroup>
    {#if columns && onColumnsChange}
      <MenuSeparator />
      <div class="display-menu__heading" aria-hidden="true">Columns</div>
      {#each EXPLORE_COLUMNS as column (column.id)}
        {@const shown = columns.includes(column.id)}
        <MenuItem
          closeOnSelect={false}
          disabled={shown && columns.length === 1}
          textValue={column.label}
          onselect={() => toggleColumn(column.id)}
        >
          {#snippet icon()}{#if shown}<Check size={12} aria-label="Shown" />{/if}{/snippet}
          {column.label}{#if shown}<span class="kit-sr-only"> shown</span>{/if}
        </MenuItem>
      {/each}
    {/if}
    {#if readerPosition && onReaderPositionChange}
      <MenuSeparator />
      <div class="display-menu__heading" aria-hidden="true">Reader position</div>
      <MenuRadioGroup
        ariaLabel="Reader position"
        value={readerPosition}
        onchange={(value) => onReaderPositionChange(value as ReaderPosition)}
      >
        <MenuRadioItem value="below">Below</MenuRadioItem>
        <MenuRadioItem value="right">Right</MenuRadioItem>
      </MenuRadioGroup>
    {/if}
    {#if overridden}
      <MenuSeparator />
      <MenuItem onselect={onUseDefaults}>Use Settings defaults</MenuItem>
    {/if}
  </MenuContent>
</Menu>

<style>
  /* Column checks line up with the radio indicators above them. */
  :global(.display-menu) {
    --menu-icon-width: 12px;
  }

  .display-menu__heading {
    padding: var(--space-2) var(--space-4) var(--space-1);
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    font-weight: var(--font-weight-semibold);
  }
</style>
