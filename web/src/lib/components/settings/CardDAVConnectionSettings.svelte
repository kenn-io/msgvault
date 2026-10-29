<script lang="ts">
  import { onMount, onDestroy, untrack } from 'svelte';
  import type { APIClient } from '../../api/client';
  import type { CardDAVAccountValues } from '../../carddav/account-settings';
  import { CardDAVController } from '../../carddav/controller.svelte';
  import CardDAVAccountSettings from './CardDAVAccountSettings.svelte';
  import CardDAVOperations from './CardDAVOperations.svelte';

  let { client, values, connection, orphaned, onSaved, onAvailabilityChange }: {
    client: APIClient;
    values: CardDAVAccountValues;
    connection: string;
    orphaned: boolean;
    onSaved: () => void | Promise<void>;
    onAvailabilityChange: (connection: string, available: boolean) => void;
  } = $props();
  const controller = new CardDAVController(untrack(() => client), undefined, untrack(() => connection));

  $effect(() => {
    onAvailabilityChange(connection, Boolean(controller.status?.available));
  });
  onMount(() => {
    if (!orphaned) void controller.load();
  });
  onDestroy(() => controller.destroy());

  async function accountSaved(): Promise<void> {
    await Promise.all([controller.load(), onSaved()]);
  }
</script>

<CardDAVAccountSettings {client} {values} {connection} onSaved={accountSaved} />
{#if !orphaned}
  <CardDAVOperations {controller} syncConnection={connection} />
{/if}
