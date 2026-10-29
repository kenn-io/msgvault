<script lang="ts">
  import { onDestroy, untrack } from 'svelte';
  import type { APIClient } from '../../api/client';
  import type { CardDAVAccountValues, AccountDraft } from '../../carddav/account-settings';
  import { CardDAVController } from '../../carddav/controller.svelte';
  import CardDAVAccountSettings from './CardDAVAccountSettings.svelte';
  import CardDAVOperations from './CardDAVOperations.svelte';

  let { client, values, connection, orphaned, onAvailabilityChange, draft, onDraft, resetRevision = 0 }: {
    draft?: AccountDraft;
    onDraft: (draft: AccountDraft | undefined) => void;
    client: APIClient;
    values: CardDAVAccountValues;
    connection: string;
    orphaned: boolean;
    resetRevision?: number;
    onAvailabilityChange: (connection: string, available: boolean) => void;
  } = $props();
  const controller = new CardDAVController(untrack(() => client), undefined, untrack(() => connection));

  $effect(() => {
    onAvailabilityChange(connection, Boolean(controller.status?.available));
  });
  $effect(() => {
    void resetRevision;
    if (!orphaned) void untrack(() => controller.load());
  });
  onDestroy(() => controller.destroy());

</script>

{#key resetRevision}
  <CardDAVAccountSettings {client} {values} {connection} {draft} {onDraft} />
{/key}
{#if !orphaned}
  <CardDAVOperations {controller} syncConnection={connection} />
{/if}
