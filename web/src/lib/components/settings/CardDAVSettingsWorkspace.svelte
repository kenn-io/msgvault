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

  const available = $derived(Boolean(controller.status?.available));

  onMount(() => { void controller.load(); });
  $effect(() => {
    if (available) untrack(() => { void conflictsController.load(); });
  });
  onDestroy(() => {
    controller.destroy();
    conflictsController.destroy();
  });

  async function accountSaved(): Promise<void> {
    const wasAvailable = available;
    await Promise.all([controller.load(), onSettingsRefresh()]);
    if (wasAvailable && available) await conflictsController.load();
  }

  $effect(() => {
    const request = cardDAVRequest;
    if (!request?.conflictID || !available) return;
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
  {#if available}<CardDAVConflicts controller={conflictsController} />{/if}
</div>

<style>
  .carddav-settings { display: grid; gap: var(--space-6); min-width: 0; }
</style>
