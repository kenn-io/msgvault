<script lang="ts">
  import { Button, SettingsSection, TextInput, Toggle } from '@kenn-io/kit-ui';
  import { onDestroy, untrack } from 'svelte';

  import type { APIClient } from '../../api/client';
  import type { components } from '../../api/generated/schema';
  import type { SettingState } from '../../settings/catalog';

  type CardDAVAccountRequest = components['schemas']['CardDAVAccountRequest'];
  type Action = 'test' | 'save';
  interface AccountSettingsSnapshot {
    baseURL: string;
    username: string;
    passwordConfigured: boolean;
    enabled: boolean;
    schedule: string;
  }

  let {
    client,
    settings,
    onSaved = () => undefined
  }: {
    client: APIClient;
    settings: SettingState[];
    onSaved?: () => void | Promise<void>;
  } = $props();

  let baseURL = $state(settingString('carddav.base_url'));
  let username = $state(settingString('carddav.username'));
  let password = $state('');
  let persistedBaseURL = $state(settingString('carddav.base_url'));
  let persistedUsername = $state(settingString('carddav.username'));
  let persistedPasswordConfigured = $state(settingSecretConfigured('carddav.password'));
  let enabled = $state(settingBoolean('carddav.enabled'));
  let schedule = $state(settingString('carddav.schedule'));
  let persistedEnabled = $state(settingBoolean('carddav.enabled'));
  let persistedSchedule = $state(settingString('carddav.schedule'));
  let activeAction = $state<Action | undefined>();
  let error = $state('');
  let status = $state('');
  let testedTuple = $state('');
  let requestController: AbortController | undefined;
  let actionGeneration = 0;
  let disposed = false;

  $effect(() => {
    const snapshot = settingsSnapshot(settings);
    untrack(() => reconcileSettings(snapshot));
  });

  $effect(() => {
    const tuple = identityTuple();
    if (testedTuple !== '' && tuple !== testedTuple) {
      testedTuple = '';
      status = '';
    }
  });

  onDestroy(() => {
    disposed = true;
    actionGeneration += 1;
    password = '';
    requestController?.abort();
    requestController = undefined;
  });

  function settingString(key: string, source: SettingState[] = settings): string {
    const value = source.find((setting) => setting.key === key)?.value;
    return value && 'string' in value ? value.string : '';
  }

  function settingBoolean(key: string, source: SettingState[] = settings): boolean {
    const value = source.find((setting) => setting.key === key)?.value;
    return Boolean(value && 'boolean' in value && value.boolean);
  }

  function settingSecretConfigured(key: string, source: SettingState[] = settings): boolean {
    return source.find((setting) => setting.key === key)?.secret?.configured === true;
  }

  function settingsSnapshot(source: SettingState[]): AccountSettingsSnapshot {
    return {
      baseURL: settingString('carddav.base_url', source),
      username: settingString('carddav.username', source),
      passwordConfigured: settingSecretConfigured('carddav.password', source),
      enabled: settingBoolean('carddav.enabled', source),
      schedule: settingString('carddav.schedule', source)
    };
  }

  function reconcileSettings(next: AccountSettingsSnapshot) {
    if (baseURL === persistedBaseURL) baseURL = next.baseURL;
    if (username === persistedUsername) username = next.username;
    if (enabled === persistedEnabled) enabled = next.enabled;
    if (schedule === persistedSchedule) schedule = next.schedule;

    persistedBaseURL = next.baseURL;
    persistedUsername = next.username;
    persistedPasswordConfigured = next.passwordConfigured;
    persistedEnabled = next.enabled;
    persistedSchedule = next.schedule;
  }

  function requestBody(): CardDAVAccountRequest {
    const body: CardDAVAccountRequest = {
      base_url: baseURL,
      username,
      enabled,
      schedule
    };
    if (password !== '') body.password = password;
    return body;
  }

  function identityTuple(): string {
    return `${baseURL}\u0000${username}`;
  }

  function canReusePersistedPassword(): boolean {
    return (
      persistedPasswordConfigured &&
      baseURL === persistedBaseURL &&
      username === persistedUsername
    );
  }

  function canDisableWithoutPassword(): boolean {
    return !enabled && baseURL === persistedBaseURL && username === persistedUsername;
  }

  function passwordRequiredForSave(): boolean {
    return !canReusePersistedPassword() && !canDisableWithoutPassword();
  }

  function validatePassword(allowCredentialFreeDisable: boolean): boolean {
    if (
      canReusePersistedPassword() ||
      password !== '' ||
      (allowCredentialFreeDisable && canDisableWithoutPassword())
    ) return true;
    status = '';
    error = 'Password is required for a new or changed CardDAV account.';
    return false;
  }

  async function testConnection() {
    if (activeAction !== undefined || !validatePassword(false)) return;
    requestController?.abort();
    const controller = new AbortController();
    requestController = controller;
    const generation = ++actionGeneration;
    activeAction = 'test';
    error = '';
    status = '';
    try {
      const { data, error: responseError } = await client.POST('/api/v1/carddav/account/test', {
        body: requestBody(),
        signal: controller.signal
      });
      if (!current(generation, controller.signal)) return;
      if (!data) throw new Error(apiErrorMessage(responseError, 'Unable to test the CardDAV connection.'));
      testedTuple = identityTuple();
      status = `Connection successful. Found ${data.books} address ${data.books === 1 ? 'book' : 'books'}.`;
    } catch (cause) {
      if (!current(generation, controller.signal)) return;
      error = cause instanceof Error ? cause.message : 'Unable to test the CardDAV connection.';
    } finally {
      if (current(generation)) {
        if (requestController === controller) requestController = undefined;
        activeAction = undefined;
      }
    }
  }

  async function saveAccount() {
    if (activeAction !== undefined || !validatePassword(true)) return;
    requestController?.abort();
    const controller = new AbortController();
    requestController = controller;
    const generation = ++actionGeneration;
    activeAction = 'save';
    error = '';
    status = '';
    try {
      const { data, error: responseError } = await client.PUT('/api/v1/carddav/account', {
        body: requestBody(),
        signal: controller.signal
      });
      if (!current(generation, controller.signal)) return;
      if (!data) throw new Error(apiErrorMessage(responseError, 'Unable to save the CardDAV account.'));
      const passwordConfigured = persistedPasswordConfigured || password !== '';
      baseURL = data.base_url;
      username = data.username;
      enabled = data.enabled;
      schedule = data.schedule ?? '';
      persistedBaseURL = data.base_url;
      persistedUsername = data.username;
      persistedPasswordConfigured = passwordConfigured;
      persistedEnabled = data.enabled;
      persistedSchedule = data.schedule ?? '';
      password = '';
      testedTuple = '';
      status = `CardDAV account saved. Found ${data.books} address ${data.books === 1 ? 'book' : 'books'}.`;
      void onSaved();
    } catch (cause) {
      if (!current(generation, controller.signal)) return;
      error = cause instanceof Error ? cause.message : 'Unable to save the CardDAV account.';
    } finally {
      if (current(generation)) {
        if (requestController === controller) requestController = undefined;
        activeAction = undefined;
      }
    }
  }

  function apiErrorMessage(responseError: unknown, fallback: string): string {
    if (typeof responseError === 'object' && responseError !== null && 'message' in responseError) {
      const message = (responseError as { message?: unknown }).message;
      if (typeof message === 'string' && message) return message;
    }
    return fallback;
  }

  function current(generation: number, signal: AbortSignal | undefined = undefined): boolean {
    return !disposed && generation === actionGeneration && !signal?.aborted;
  }
</script>

<SettingsSection
  title="CardDAV account"
  description={passwordRequiredForSave()
    ? 'Connect an address-book account. A password is required for a new or changed account.'
    : canReusePersistedPassword()
      ? 'Connect an address-book account. Leave the password blank to keep the stored credential.'
      : 'Disable an existing address-book account without re-entering its password.'}
>
  {#if error}<p class="error" role="alert">{error}</p>{/if}
  {#if status}<p class="status" role="status">{status}</p>{/if}

  <form onsubmit={(event) => { event.preventDefault(); void saveAccount(); }}>
    <label>
      Base URL
      <TextInput type="url" bind:value={baseURL} disabled={activeAction !== undefined} required block />
    </label>
    <label>
      Username
      <TextInput autocomplete="username" bind:value={username} disabled={activeAction !== undefined} required block />
    </label>
    <label>
      Password
      <TextInput
        type="password"
        autocomplete="current-password"
        bind:value={password}
        disabled={activeAction !== undefined}
        required={passwordRequiredForSave()}
        placeholder={canReusePersistedPassword()
          ? 'Leave blank to keep current password'
          : canDisableWithoutPassword() ? 'Not required while disabled' : ''}
        block
      />
    </label>
    <Toggle bind:checked={enabled} disabled={activeAction !== undefined} label="Enabled" />
    <label>
      Schedule
      <TextInput bind:value={schedule} disabled={activeAction !== undefined} placeholder="0 2 * * *" block />
    </label>

    <div class="actions">
      <Button
        disabled={activeAction !== undefined}
        label={activeAction === 'test' ? 'Testing…' : 'Test CardDAV connection'}
        onclick={() => void testConnection()}
      />
      <Button
        type="submit"
        disabled={activeAction !== undefined}
        tone="success"
        surface="solid"
        label={activeAction === 'save' ? 'Saving…' : 'Save CardDAV account'}
      />
    </div>
  </form>
</SettingsSection>

<style>
  form { display: grid; gap: var(--space-5); }
  label { display: grid; gap: var(--space-2); color: var(--text-secondary); font-size: var(--font-size-sm); font-weight: 500; }
  .actions { display: flex; flex-wrap: wrap; gap: var(--space-3); }
  .error, .status { margin: 0; padding: var(--space-3) var(--space-4); border: 1px solid; border-radius: var(--radius-md); }
  .error { border-color: var(--status-error-ink); background: var(--status-error-bg); color: var(--status-error-ink); }
  .status { border-color: var(--status-success-ink); background: var(--status-success-bg); color: var(--status-success-ink); }
</style>
