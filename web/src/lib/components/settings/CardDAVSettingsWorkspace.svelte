<script lang="ts">
  import { onDestroy, onMount, untrack } from 'svelte';

  import { Button, SelectDropdown, TextInput } from '@kenn-io/kit-ui';
  import { listCardDAVConnections } from '../../api/generated/api/api';
  import type { CardDAVConnectionResponse } from '../../api/generated/models';
  import type { APIClient } from '../../api/client';
  import type { CardDAVAccountValues } from '../../carddav/account-settings';
  import type { CardDAVSettingsRequest } from '../../carddav/navigation';
  import { CardDAVConflictsController } from '../../carddav/conflicts-controller.svelte';
  import type { SettingState } from '../../settings/catalog';
  import CardDAVConnectionSettings from './CardDAVConnectionSettings.svelte';
  import CardDAVConflicts from './CardDAVConflicts.svelte';

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
  const conflictsController = new CardDAVConflictsController(untrack(() => client));

  let connections = $state<CardDAVConnectionResponse[]>([]);
  let selected = $state('default');
  let newName = $state('');
  let newConnection = $state<string>();
  let connectionError = $state('');
  let connectionAbort: AbortController | undefined;
  let connectionGeneration = 0;
  let disposed = false;
  const connectionOptions = $derived.by(() => {
    const names = new Set(['default', ...connections.map(item => item.connection)]);
    if (newConnection) names.add(newConnection);
    return [...names].map(value => ({
      value,
      label: connections.find(item => item.connection === value)?.orphaned
        ? `${value} (configuration missing)`
        : value
    }));
  });
  const selectedConnection = $derived(connections.find(item => item.connection === selected));
  const selectedValues = $derived.by((): CardDAVAccountValues => {
    if (selectedConnection) {
      return {
        provider: selectedConnection.provider ?? '',
        oauthApp: selectedConnection.oauth_app ?? '',
        baseURL: selectedConnection.status.account?.base_url ?? '',
        username: selectedConnection.status.account?.username ?? '',
        passwordConfigured: selectedConnection.status.credential_configured,
        enabled: selectedConnection.status.enabled,
        schedule: selectedConnection.status.schedule
      };
    }
    // The settings catalog supplies the default form until connection summaries load.
    const catalog = selected === 'default' ? settings : [];
    function stringValue(key: string): string {
      const value = catalog.find(setting => setting.key === key)?.value;
      return value && 'string' in value ? value.string : '';
    }
    const enabled = catalog.find(setting => setting.key === 'carddav.enabled')?.value;
    return {
      provider: stringValue('carddav.provider'),
      oauthApp: stringValue('carddav.oauth_app'),
      baseURL: stringValue('carddav.base_url'),
      username: stringValue('carddav.username'),
      passwordConfigured: catalog.find(setting => setting.key === 'carddav.password')?.secret?.configured === true,
      enabled: Boolean(enabled && 'boolean' in enabled && enabled.boolean),
      schedule: stringValue('carddav.schedule')
    };
  });

  async function loadConnections(): Promise<void> {
    connectionAbort?.abort();
    const request = new AbortController();
    connectionAbort = request;
    const generation = ++connectionGeneration;
    try {
      const result = await listCardDAVConnections({ ...client, signal: request.signal });
      if (disposed || generation !== connectionGeneration) return;
      if (!result.data) throw new Error('Unable to load CardDAV connections.');
      connections = result.data.connections;
      availability = Object.fromEntries(connections.map(item => [item.connection, item.status.available]));
      connectionError = '';
    } catch {
      if (!disposed && generation === connectionGeneration && !request.signal.aborted) {
        connectionError = 'Unable to load CardDAV connections.';
      }
    }
  }
  function addConnection(): void {
    if (!/^[a-z][a-z0-9_-]{0,63}$/.test(newName) || newName === 'default') {
      connectionError = 'Choose a connection name with 1–64 lowercase letters, digits, underscores or hyphens, starting with a letter. The name default is reserved.';
      return;
    }
    if (connections.some(item => item.connection === newName)) {
      connectionError = 'That connection already exists. Select it to edit its settings.';
      return;
    }
    newConnection = newName;
    selected = newName;
    newName = '';
    connectionError = '';
  }

  let availability = $state<Record<string, boolean>>({});
  const available = $derived(Object.values(availability).some(Boolean));
  onMount(() => {
    void loadConnections();
  });
  $effect(() => {
    if (available) {
      untrack(() => { void conflictsController.load(); });
    } else {
      conflictsController.focusRequest = undefined;
    }
  });
  onDestroy(() => {
    disposed = true;
    connectionGeneration += 1;
    connectionAbort?.abort();
    conflictsController.destroy();
  });

  async function accountSaved(): Promise<void> {
    const wasAvailable = available;
    await Promise.all([loadConnections(), onSettingsRefresh()]);
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
  <div class="connection-picker">
    <SelectDropdown
      title="CardDAV connection"
      value={selected}
      options={connectionOptions}
      onchange={(value) => { selected = value; }}
    />
    <label>New connection name <TextInput bind:value={newName} placeholder="work" /></label>
    <Button label="Add connection" onclick={addConnection} />
    {#if connectionError}
      <p role="alert">
        {connectionError}
        <Button label="Retry connections" onclick={() => void loadConnections()} />
      </p>
    {/if}
  </div>
  {#if selectedConnection?.orphaned}
    <p role="status">
      Configuration is missing for connection “{selected}”. Its archived contacts and history are retained.
      Choose the provider, reconnect, and save this connection to restore it with the same name.
    </p>
  {/if}
  {#key selected}
    <CardDAVConnectionSettings
      {client}
      values={selectedValues}
      connection={selected}
      orphaned={selectedConnection?.orphaned === true}
      onSaved={accountSaved}
      onAvailabilityChange={(name, value) => { availability[name] = value; }}
    />
  {/key}
  {#if available}<CardDAVConflicts controller={conflictsController} />{/if}
</div>

<style>
  .connection-picker {
    display: grid;
    gap: var(--space-3);
    min-width: 0;
  }

  .connection-picker label {
    display: grid;
    gap: var(--space-2);
  }

  .carddav-settings {
    display: grid;
    gap: var(--space-6);
    min-width: 0;
  }
</style>
