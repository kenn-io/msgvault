<script lang="ts">
  import { KbdBadge, Tooltip } from '@kenn-io/kit-ui';
  import CircleHelp from '@lucide/svelte/icons/circle-help';
  import PanelLeftClose from '@lucide/svelte/icons/panel-left-close';
  import PanelLeftOpen from '@lucide/svelte/icons/panel-left-open';

  import type { SavedView } from '../../api/generated/models';
  import type { ExploreWorkspace } from '../../explore/models';
  import SavedViewRow from './SavedViewRow.svelte';
  import type { JobsStore } from '../../jobs/jobs.svelte';
  import type { JobTarget } from '../../jobs/jobs';
  import { viewIsFiles } from './shell-data.svelte';
  import SyncStatusMenu from './SyncStatusMenu.svelte';
  import { NAVIGATION_ITEMS, SETTINGS_ITEM, type NavigationItem } from './navigation';

  interface Props {
    onPastRuns: () => void;
    active: ExploreWorkspace;
    collapsed: boolean;
    showCollapseToggle: boolean;
    jobs: JobsStore;
    onOpenJob: (target: JobTarget) => void;
    views?: SavedView[];
    onNavigate: (id: ExploreWorkspace) => void;
    onToggleCollapsed: () => void;
    onOpenHelp: () => void;
    onOpenView?: (view: SavedView) => void;
    onRenameView?: (view: SavedView, name: string) => Promise<string | undefined>;
    onDescribeView?: (view: SavedView, description: string) => Promise<string | undefined>;
    onDeleteView?: (view: SavedView) => void;
  }

  let {
    onPastRuns,
    active,
    collapsed,
    showCollapseToggle,
    jobs,
    onOpenJob,
    views = [],
    onNavigate,
    onToggleCollapsed,
    onOpenHelp,
    onOpenView = () => undefined,
    onRenameView = async () => undefined,
    onDescribeView = undefined,
    onDeleteView = () => undefined,
  }: Props = $props();

  const SettingsIcon = SETTINGS_ITEM.icon;
  const items = NAVIGATION_ITEMS;
  const messageViews = $derived(views.filter((view) => !viewIsFiles(view)));
  const fileViews = $derived(views.filter(viewIsFiles));

  function nestedViews(item: NavigationItem): SavedView[] {
    if (item.id === 'everything') return messageViews;
    if (item.id === 'files') return fileViews;
    return [];
  }
</script>

<aside class="sidebar" class:sidebar--rail={collapsed} aria-label="Sidebar">
  <div class="sidebar__brand">msgvault</div>
  <nav aria-label="Primary">
    <ul>
      {#each items as item (item.id)}
        {@const Icon = item.icon}
        {@const nested = collapsed ? [] : nestedViews(item)}
        <li>
          <Tooltip text={item.label} side={collapsed ? 'right' : 'bottom'}>
            <button
              type="button"
              class="sidebar__item"
              aria-label={item.label}
              aria-current={(item.id === active || item.id === 'relationships' && (active === 'directory' || active === 'directory_review')) ? 'page' : undefined}
              onclick={() => onNavigate(item.id)}
            >
              <Icon size={18} aria-hidden="true" />
              {#if !collapsed}<span class="sidebar__label">{item.label}</span>{/if}
            </button>
          </Tooltip>
          {#if nested.length > 0}
            <ul class="sidebar__views" aria-label={`${item.label} saved views`}>
              {#each nested as view (view.id)}
                <li><SavedViewRow {view} onOpen={onOpenView} onRename={onRenameView} onDescribe={onDescribeView} onDelete={onDeleteView} /></li>
              {/each}
            </ul>
          {/if}
        </li>
      {/each}
    </ul>
  </nav>
  <div class="sidebar__footer">
    <Tooltip text={SETTINGS_ITEM.label} side={collapsed ? 'right' : 'bottom'}>
      <button
        type="button"
        class="sidebar__item sidebar__item--footer"
        aria-label={SETTINGS_ITEM.label}
        aria-current={active === 'settings' ? 'page' : undefined}
        onclick={() => onNavigate('settings')}
      >
        <SettingsIcon size={18} aria-hidden="true" />
        {#if !collapsed}<span class="sidebar__label">{SETTINGS_ITEM.label}</span>{/if}
      </button>
    </Tooltip>
    <Tooltip text="Help" side={collapsed ? 'right' : 'bottom'}>
      <button type="button" class="sidebar__item sidebar__item--footer" aria-label="Help" onclick={onOpenHelp}>
        <CircleHelp size={18} aria-hidden="true" />
        {#if !collapsed}<span class="sidebar__label">Help</span><KbdBadge keys={['?']} />{/if}
      </button>
    </Tooltip>
    <SyncStatusMenu {onPastRuns} store={jobs} {collapsed} onOpen={onOpenJob} />
    {#if showCollapseToggle}
      <Tooltip text={collapsed ? 'Expand sidebar' : 'Collapse sidebar'} side={collapsed ? 'right' : 'bottom'}>
        <button
          type="button"
          class="sidebar__item sidebar__item--footer"
          aria-label={collapsed ? 'Expand sidebar' : 'Collapse sidebar'}
          onclick={onToggleCollapsed}
        >
          {#if collapsed}
            <PanelLeftOpen size={18} aria-hidden="true" />
          {:else}
            <PanelLeftClose size={18} aria-hidden="true" /><span class="sidebar__label">Collapse</span>
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
    padding: var(--space-3) var(--space-3);
    overflow-y: auto;
    background: var(--bg-surface);
    border-right: 1px solid var(--border-default);
  }

  .sidebar--rail {
    width: var(--nav-rail-width);
    align-items: center;
    padding-inline: var(--space-3);
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
    padding: var(--space-2) var(--pane-pad-x);
    color: var(--text-primary);
    font-size: var(--font-size-md);
    font-weight: 650;
  }

  .sidebar--rail .sidebar__brand {
    visibility: hidden;
  }

  nav {
    flex: 1;
    padding-top: var(--space-3);
  }

  ul {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    margin: 0;
    padding: 0;
    list-style: none;
  }

  .sidebar__views {
    margin: var(--space-1) 0 var(--space-3) calc(var(--pane-pad-x) + var(--space-4));
    border-left: 1px solid var(--border-muted);
  }

  .sidebar__item {
    display: flex;
    width: 100%;
    min-height: var(--control-h-lg);
    align-items: center;
    gap: var(--control-gap);
    padding: 0 var(--pane-pad-x);
    border: 0;
    border-radius: var(--radius-md);
    background: transparent;
    color: var(--text-secondary);
    font-size: var(--font-size-md);
    text-align: left;
    cursor: pointer;
  }

  .sidebar__item--footer {
    min-height: var(--control-h);
    font-size: var(--font-size-sm);
  }

  .sidebar--rail .sidebar__item {
    width: var(--control-h-lg);
    min-height: var(--control-h-lg);
    justify-content: center;
    padding: 0;
  }

  .sidebar__label {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
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

  .sidebar--rail li {
    position: relative;
  }

  .sidebar__footer {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    padding-top: var(--space-3);
    border-top: 1px solid var(--border-muted);
  }
</style>
