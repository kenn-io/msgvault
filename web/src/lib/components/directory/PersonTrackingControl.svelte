<script lang="ts">
  import { MenuItem } from '@kenn-io/kit-ui';
  import MenuSwitchItem from '../common/MenuSwitchItem.svelte';
  import { formatShortDate } from '../../util/format';
  import type { PersonTrackingController } from '../../directory/person-tracking-controller.svelte';

  interface Props {
    /** Owned by the menu's parent, so a save started here finishes and is announced after the menu closes. */
    controller: PersonTrackingController;
    /** Set when lookups aren't set up: an unenrolled person's item reads "Set up…", and an enrolled one keeps its switch beside "Set up people lookups…". */
    onSetUp?: () => void;
    onAnnounce?: (message: string) => void;
  }

  let { controller, onSetUp, onAnnounce = () => undefined }: Props = $props();

  // Every outcome is announced, since the menu may have closed before the save finished.
  async function changeTracking(desired: boolean | null): Promise<void> {
    const context = controller.contextToken;
    const outcome = desired === null ? await controller.retrySave() : await controller.setTracked(desired);
    if (!controller.isContextCurrent(context) || outcome.kind === 'ignored') return;
    onAnnounce(outcome.kind === 'unknown' ? "Couldn't check whether profile updates were saved."
      : controller.announcement ?? "Couldn't save profile updates.");
  }
  // The switch shows on or off; the line adds only since when.
  const status = $derived(controller.tracking?.tracked && controller.tracking.tracked_at ? `Since ${formatShortDate(controller.tracking.tracked_at)}` : '');
  const error = $derived(controller.failedSave !== null ? "Couldn't save" : controller.trackingError ? "Couldn't check" : '');
  // A failed save sends the same change again; a state that couldn't be read is read again.
  function retry(): void {
    if (controller.failedSave !== null) void changeTracking(null);
    else void controller.retryTracking();
  }

  // Only a settled "off" gives up the switch; an enrolled person keeps it so tracking can still be turned off.
  const setupOnly = $derived(!!onSetUp && controller.tracking?.tracked === false && !error && !controller.pending && !controller.trackingLoading);
</script>

{#if setupOnly}
  <MenuItem textValue="Keep this profile updated" onselect={() => onSetUp?.()}>
    <span class="menu-switch-copy"><span>Keep this profile updated</span><small>Set up…</small></span>
  </MenuItem>
{:else}
  <MenuSwitchItem label="Keep this profile updated" checked={controller.tracking?.tracked ?? false}
    status={controller.trackingLoading ? 'Checking…' : status} busy={controller.pending} loading={controller.trackingLoading}
    {error} onretry={retry} onchange={(checked) => void changeTracking(checked)} />
  {#if onSetUp}<MenuItem textValue="Set up people lookups…" onselect={onSetUp}>Set up people lookups…</MenuItem>{/if}
{/if}
