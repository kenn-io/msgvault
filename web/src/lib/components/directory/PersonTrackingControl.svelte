<script lang="ts">
  import MenuSwitchItem from '../common/MenuSwitchItem.svelte';
  import { formatShortDate } from '../../util/format';
  import type { PersonTrackingController } from '../../directory/person-tracking-controller.svelte';

  interface Props {
    /** Owned by the menu's parent, so a save started here finishes and is announced after the menu closes. */
    controller: PersonTrackingController;
    /** Set when lookups aren't set up: the item reads "Set up…" and opens their settings. */
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
</script>

<MenuSwitchItem label="Keep this profile updated" checked={controller.tracking?.tracked ?? false}
  status={controller.trackingLoading ? 'Checking…' : status} busy={controller.pending} loading={controller.trackingLoading}
  {error} onretry={retry} onsetup={controller.tracking?.tracked === false && !error && !controller.pending && !controller.trackingLoading ? onSetUp : undefined}
  onchange={(checked) => void changeTracking(checked)} />
