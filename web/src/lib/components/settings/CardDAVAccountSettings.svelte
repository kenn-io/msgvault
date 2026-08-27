<script lang="ts">
  import { Button, SettingsSection, TextInput, Toggle } from '@kenn-io/kit-ui';

  import type { APIClient } from '../../api/client';
  import type { components } from '../../api/generated/schema';
  import type { SettingState } from '../../settings/catalog';

  type CardDAVAccountRequest = components['schemas']['CardDAVAccountRequest'];
  type CardDAVAccountPayload = Omit<CardDAVAccountRequest, 'password'> & { password?: string };
  type Action = 'test' | 'save';

  let { client, settings }: { client: APIClient; settings: SettingState[] } = $props();

  let baseURL = $state(settingString('carddav.base_url'));
  let username = $state(settingString('carddav.username'));
  let password = $state('');
  let persistedBaseURL = $state(settingString('carddav.base_url'));
  let persistedUsername = $state(settingString('carddav.username'));
  let persistedPasswordConfigured = $state(settingSecretConfigured('carddav.password'));
  let enabled = $state(settingBoolean('carddav.enabled'));
  let schedule = $state(settingString('carddav.schedule'));
  let activeAction = $state<Action | undefined>();
  let error = $state('');
  let status = $state('');

  function settingString(key: string): string {
    const value = settings.find((setting) => setting.key === key)?.value;
    return value && 'string' in value ? value.string : '';
  }

  function settingBoolean(key: string): boolean {
    const value = settings.find((setting) => setting.key === key)?.value;
    return Boolean(value && 'boolean' in value && value.boolean);
  }

  function settingSecretConfigured(key: string): boolean {
    return settings.find((setting) => setting.key === key)?.secret?.configured === true;
  }

  function requestBody(): CardDAVAccountPayload {
    const body: CardDAVAccountPayload = {
      base_url: baseURL,
      username,
      enabled,
      schedule
    };
    if (password !== '') body.password = password;
    return body;
  }

  function canReusePersistedPassword(): boolean {
    return (
      persistedPasswordConfigured &&
      baseURL === persistedBaseURL &&
      username === persistedUsername
    );
  }

  function validatePassword(): boolean {
    if (canReusePersistedPassword() || password !== '') return true;
    status = '';
    error = 'Password is required for a new or changed CardDAV account.';
    return false;
  }

  async function testConnection() {
    if (!validatePassword()) return;
    activeAction = 'test';
    error = '';
    status = '';
    try {
      const { data, error: responseError } = await client.POST('/api/v1/carddav/account/test', {
        // The server keeps the stored credential when password is omitted; generated types may lag that contract.
        body: requestBody() as CardDAVAccountRequest
      });
      if (!data) throw new Error(apiErrorMessage(responseError, 'Unable to test the CardDAV connection.'));
      status = `Connection successful. Found ${data.books} address ${data.books === 1 ? 'book' : 'books'}.`;
    } catch (cause) {
      error = cause instanceof Error ? cause.message : 'Unable to test the CardDAV connection.';
    } finally {
      activeAction = undefined;
    }
  }

  async function saveAccount() {
    if (!validatePassword()) return;
    activeAction = 'save';
    error = '';
    status = '';
    try {
      const { data, error: responseError } = await client.PUT('/api/v1/carddav/account', {
        // The server keeps the stored credential when password is omitted; generated types may lag that contract.
        body: requestBody() as CardDAVAccountRequest
      });
      if (!data) throw new Error(apiErrorMessage(responseError, 'Unable to save the CardDAV account.'));
      baseURL = data.base_url;
      username = data.username;
      enabled = data.enabled;
      schedule = data.schedule ?? '';
      persistedBaseURL = data.base_url;
      persistedUsername = data.username;
      persistedPasswordConfigured = true;
      password = '';
      status = `CardDAV account saved. Found ${data.books} address ${data.books === 1 ? 'book' : 'books'}.`;
    } catch (cause) {
      error = cause instanceof Error ? cause.message : 'Unable to save the CardDAV account.';
    } finally {
      activeAction = undefined;
    }
  }

  function apiErrorMessage(responseError: unknown, fallback: string): string {
    if (typeof responseError === 'object' && responseError !== null && 'message' in responseError) {
      const message = (responseError as { message?: unknown }).message;
      if (typeof message === 'string' && message) return message;
    }
    return fallback;
  }
</script>

<SettingsSection
  title="CardDAV account"
  description={canReusePersistedPassword()
    ? 'Connect an address-book account. Leave the password blank to keep the stored credential.'
    : 'Connect an address-book account. A password is required for a new or changed account.'}
>
  {#if error}<p class="error" role="alert">{error}</p>{/if}
  {#if status}<p class="status" role="status">{status}</p>{/if}

  <form onsubmit={(event) => { event.preventDefault(); void saveAccount(); }}>
    <label>
      Base URL
      <TextInput type="url" bind:value={baseURL} required block />
    </label>
    <label>
      Username
      <TextInput autocomplete="username" bind:value={username} required block />
    </label>
    <label>
      Password
      <TextInput
        type="password"
        autocomplete="current-password"
        bind:value={password}
        required={!canReusePersistedPassword()}
        placeholder={canReusePersistedPassword() ? 'Leave blank to keep current password' : ''}
        block
      />
    </label>
    <Toggle bind:checked={enabled} label="Enabled" />
    <label>
      Schedule
      <TextInput bind:value={schedule} placeholder="0 2 * * *" block />
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
