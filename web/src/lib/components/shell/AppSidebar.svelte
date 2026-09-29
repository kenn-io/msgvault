<script lang="ts">
  import { KbdBadge, StatusDot, Tooltip } from '@kenn-io/kit-ui';
  import Keyboard from '@lucide/svelte/icons/keyboard';
  import PanelLeftClose from '@lucide/svelte/icons/panel-left-close';
  import PanelLeftOpen from '@lucide/svelte/icons/panel-left-open';

  import type { ExploreWorkspace } from '../../explore/models';
  import { NAVIGATION_GROUPS } from './navigation';

  interface Props {
    active: ExploreWorkspace;
    collapsed: boolean;
    showCollapseToggle: boolean;
    status: { tone: 'working' | 'idle' | 'unclean'; label: string; text: string };
    onNavigate: (id: ExploreWorkspace) => void;
    onToggleCollapsed: () => void;
    onOpenShortcuts: () => void;
  }

  let {
    active,
    collapsed,
    showCollapseToggle,
    status,
    onNavigate,
    onToggleCollapsed,
    onOpenShortcuts
  }: Props = $props();

  const uid = $props.id();
  const groupId = `nav-group-${uid}`;
</script>

<aside class="sidebar" class:sidebar--rail={collapsed} aria-label="Sidebar">
  <div class="sidebar__brand">msgvault</div>
  <nav aria-label="Primary">
    {#each NAVIGATION_GROUPS as group, index (group.label)}
      {#if collapsed}
        {#if index > 0}<hr class="sidebar__divider" />{/if}
      {:else}
        <p class="sidebar__group" id="{groupId}-{index}">{group.label}</p>
      {/if}
      <ul
        aria-label={collapsed ? group.label : undefined}
        aria-labelledby={collapsed ? undefined : `${groupId}-${index}`}
      >
        {#each group.items as item (item.id)}
          {@const Icon = item.icon}
          <li>
            <Tooltip text={item.label}>
              <button
                type="button"
                class="sidebar__item"
                aria-label={item.label}
                aria-current={item.id === active ? 'page' : undefined}
                onclick={() => onNavigate(item.id)}
              >
                <Icon size={18} aria-hidden="true" />
                {#if !collapsed}<span>{item.label}</span>{/if}
              </button>
            </Tooltip>
          </li>
        {/each}
      </ul>
    {/each}
  </nav>
  <div class="sidebar__footer">
    <Tooltip text={status.label}>
      <span class="sidebar__status">
        <span aria-hidden="true"><StatusDot status={status.tone} label={status.label} /></span>
        <span class:kit-sr-only={collapsed}>{collapsed ? status.label : status.text}</span>
      </span>
    </Tooltip>
    <Tooltip text="Keyboard shortcuts">
      <button
        type="button"
        class="sidebar__item"
        aria-label="Keyboard shortcuts"
        onclick={onOpenShortcuts}
      >
        <Keyboard size={18} aria-hidden="true" />
        {#if !collapsed}<span>Keyboard shortcuts</span><KbdBadge keys={['?']} />{/if}
      </button>
    </Tooltip>
    {#if showCollapseToggle}
      <Tooltip text={collapsed ? 'Expand sidebar' : 'Collapse sidebar'}>
        <button
          type="button"
          class="sidebar__item"
          aria-label={collapsed ? 'Expand sidebar' : 'Collapse sidebar'}
          onclick={onToggleCollapsed}
        >
          {#if collapsed}
            <PanelLeftOpen size={18} aria-hidden="true" />
          {:else}
            <PanelLeftClose size={18} aria-hidden="true" /><span>Collapse</span>
          {/if}
        </button>
      </Tooltip>
    {/if}
  </div>
</aside>

<style>
  .sidebar {
    display: flex;
    width: var(--nav-width);
    height: 100%;
    flex-direction: column;
    gap: var(--space-3);
    padding: var(--space-3) var(--space-2);
    overflow-y: auto;
    background: var(--bg-surface);
    border-right: 1px solid var(--border-default);
  }

  .sidebar--rail {
    width: var(--nav-rail-width);
    align-items: center;
  }

  /* The same buttons serve both modes so focus survives a toggle; labels show as tooltips only in the rail. */
  .sidebar :global(.kit-tooltip-trigger) {
    display: flex;
    width: 100%;
  }

  .sidebar--rail :global(.kit-tooltip-trigger) {
    width: auto;
  }

  .sidebar:not(.sidebar--rail) :global(.kit-tooltip) {
    display: none;
  }

  .sidebar__brand {
    padding: var(--space-1) var(--space-3);
    color: var(--text-primary);
    font-size: var(--font-size-md);
    font-weight: 650;
  }

  .sidebar--rail .sidebar__brand {
    visibility: hidden;
  }

  nav {
    display: flex;
    flex: 1;
    flex-direction: column;
    gap: var(--space-1);
  }

  ul {
    display: flex;
    flex-direction: column;
    gap: 2px;
    margin: 0;
    padding: 0;
    list-style: none;
  }

  .sidebar__group {
    margin: var(--space-3) 0 var(--space-1);
    padding: 0 var(--space-3);
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    font-weight: 600;
  }

  .sidebar__divider {
    width: 24px;
    margin: var(--space-2) auto;
    border: 0;
    border-top: 1px solid var(--border-default);
  }

  .sidebar__item {
    display: flex;
    width: 100%;
    min-height: 32px;
    align-items: center;
    gap: var(--space-3);
    padding: 0 var(--space-3);
    border: 0;
    border-radius: var(--radius-md);
    background: transparent;
    color: var(--text-secondary);
    font-size: var(--font-size-md);
    text-align: left;
    cursor: pointer;
  }

  .sidebar--rail .sidebar__item {
    width: 36px;
    justify-content: center;
    padding: 0;
  }

  .sidebar__item:hover {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  .sidebar__item[aria-current='page'] {
    background: var(--nav-active-bg);
    color: var(--text-primary);
    font-weight: 600;
  }

  .sidebar__item :global(.kit-kbd-badge) {
    margin-left: auto;
  }

  .sidebar__footer {
    display: flex;
    flex-direction: column;
    gap: 2px;
    padding-top: var(--space-2);
    border-top: 1px solid var(--border-muted);
  }

  .sidebar__status {
    display: flex;
    min-height: 28px;
    align-items: center;
    gap: var(--space-2);
    padding: 0 var(--space-3);
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }
</style>
