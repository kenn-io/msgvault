<script lang="ts">
  import { Menu, MenuContent, MenuItem, MenuSeparator, MenuTrigger, StatusDot, Tooltip } from '@kenn-io/kit-ui';

  import type { JobsStore } from '../../jobs/jobs.svelte';
  import type { JobTarget } from '../../jobs/jobs';
  import JobsPopover from './JobsPopover.svelte';

  interface Props {
    onPastRuns: () => void;
    store: JobsStore;
    collapsed: boolean;
    onOpen: (target: JobTarget) => void;
  }

  let { onPastRuns, store, collapsed, onOpen }: Props = $props();

  const STATUS = {
    unavailable: { tone: 'stale', label: 'Status unavailable' },
    ok: { tone: 'idle', label: 'Idle' },
    running: { tone: 'stale', label: 'Syncing' },
    failed: { tone: 'unclean', label: 'Needs attention' },
  } as const;
  const status = $derived(STATUS[store.health]);
</script>

<Menu align="start" onopenchange={(open) => open && void store.refresh()}>
  <Tooltip text={status.label} focusable={false} side={collapsed ? 'right' : 'bottom'}>
    <MenuTrigger ariaLabel={`Sync status: ${status.label}`}>
      {#snippet child({ attachment })}
        <button type="button" class="sync-status" class:sync-status--rail={collapsed} {@attach attachment}>
          <span class="sync-status__dot" aria-hidden="true"><StatusDot status={status.tone} label={status.label} /></span>
          <span class:kit-sr-only={collapsed}>{status.label}</span>
        </button>
      {/snippet}
    </MenuTrigger>
  </Tooltip>
  <MenuContent ariaLabel="Running jobs and failures">
    <JobsPopover {store} {onOpen} />
      <MenuSeparator />
      <MenuItem textValue="Past runs" onselect={onPastRuns}>Past runs…</MenuItem>
  </MenuContent>
</Menu>


<style>
  .sync-status {
    display: flex;
    width: 100%;
    min-height: var(--control-h);
    align-items: center;
    gap: var(--control-gap);
    padding: 0 var(--pane-pad-x);
    border: 0;
    border-radius: var(--radius-md);
    background: transparent;
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
    text-align: left;
    cursor: pointer;
  }

  /* Centered on the same column as the icons above it. */
  .sync-status__dot {
    display: flex;
    width: 18px;
    flex: none;
    justify-content: center;
  }

  .sync-status:hover {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  .sync-status--rail {
    width: var(--control-h-lg);
    min-height: var(--control-h-lg);
    justify-content: center;
    padding: 0;
  }
</style>
