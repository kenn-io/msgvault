<script lang="ts">
  import { onDestroy, onMount, untrack } from 'svelte';

  import type { APIClient } from '../../api/client';
  import type { CardDAVSettingsRequest } from '../../carddav/navigation';
  import { CardDAVController } from '../../carddav/controller.svelte';
  import { CardDAVConflictsController } from '../../carddav/conflicts-controller.svelte';
  import type { SettingState } from '../../settings/catalog';
  import CardDAVAccountSettings from './CardDAVAccountSettings.svelte';
  import CardDAVConflicts from './CardDAVConflicts.svelte';
  import CardDAVOperations from './CardDAVOperations.svelte';

  let {
    client,
    settings,
    onSettingsRefresh = () => undefined,
    cardDAVRequest = undefined,
    onCardDAVRequestConsumed = () => undefined
  }: {
    client: APIClient;
    settings: SettingState[];
    onSettingsRefresh?: () => void | Promise<void>;
    cardDAVRequest?: CardDAVSettingsRequest;
    onCardDAVRequestConsumed?: (key: number) => void;
  } = $props();
  const controller = new CardDAVController(untrack(() => client));
  const conflictsController = new CardDAVConflictsController(untrack(() => client));

  onMount(() => { void Promise.all([controller.load(), conflictsController.load()]); });
  onDestroy(() => {
    controller.destroy();
    conflictsController.destroy();
  });

  async function accountSaved(): Promise<void> {
    await Promise.all([controller.load(), conflictsController.load(), onSettingsRefresh()]);
  }

  $effect(() => {
    const request = cardDAVRequest;
    if (!request?.conflictID) return;
    untrack(() => {
      void conflictsController.openRequestedConflict({
        conflictID: request.conflictID!,
        key: request.key
      }).then((consumed) => {
        if (consumed) onCardDAVRequestConsumed(request.key);
      });
    });
  });
</script>

<div class="carddav-settings" aria-label="CardDAV settings">
  <CardDAVAccountSettings {client} {settings} onSaved={accountSaved} />
  <CardDAVOperations {controller} />
  <CardDAVConflicts controller={conflictsController} />
</div>

<style>
  .carddav-settings { display: grid; gap: var(--space-6); min-width: 0; }
</style>
