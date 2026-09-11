<script lang="ts">
  import {
    saveCardDAVAccount as generatedSaveCardDAVAccount,
    testCardDAVAccount as generatedTestCardDAVAccount,
  } from '../../api/generated/api/api';
  import { Button, SettingsSection, TextInput, Toggle, SelectDropdown } from '@kenn-io/kit-ui';
  import ZapIcon from '@lucide/svelte/icons/zap';
  import { onDestroy, untrack } from 'svelte';
  import type { APIClient } from '../../api/client';
  import type { CardDAVAccountRequest as GeneratedCardDAVAccountRequest } from '../../api/generated/models';
  import { authorizeGoogleContacts } from '../../settings/google-authorization';
  import type { SettingState } from '../../settings/catalog';
  import CronField from './CronField.svelte';
  type CardDAVAccountRequest = GeneratedCardDAVAccountRequest;
  type Action = 'test' | 'save' | 'authorize';
  interface AccountSettingsSnapshot {
    provider: string;
    oauthApp: string;
    baseURL: string;
    username: string;
    passwordConfigured: boolean;
    enabled: boolean;
    schedule: string;
  }
  let {
    client,
    settings,
    onSaved = () => undefined,
  }: {
    client: APIClient;
    settings: SettingState[];
    onSaved?: () => void | Promise<void>;
  } = $props();
  let provider = $state(settingString('carddav.provider'));
  let oauthApp = $state(settingString('carddav.oauth_app'));
  let persistedProvider = $state(settingString('carddav.provider'));
  let persistedOAuthApp = $state(settingString('carddav.oauth_app'));
  const googleURL = 'https://www.googleapis.com/.well-known/carddav';
  const google = $derived(provider === 'google');
  function shellQuote(value: string): string { return "'" + value.replaceAll("'", "'\\''") + "'"; }
  function selectProvider(value: string) {
    provider = value;
    password = '';
    if (value !== 'google') oauthApp = '';
  }
  let baseURL = $state(settingString('carddav.base_url'));
  let username = $state(settingString('carddav.username'));
  const authorizationCommand = $derived(`msgvault carddav authorize-google ${shellQuote(username || 'you@example.com')}${oauthApp ? ` --oauth-app ${shellQuote(oauthApp)}` : ''}`);
  let password = $state('');
  let persistedBaseURL = $state(settingString('carddav.base_url'));
  let persistedUsername = $state(settingString('carddav.username'));
  let persistedPasswordConfigured = $state(settingSecretConfigured('carddav.password'));
  let enabled = $state(settingBoolean('carddav.enabled'));
  let schedule = $state(settingString('carddav.schedule'));
  let persistedEnabled = $state(settingBoolean('carddav.enabled'));
  let persistedSchedule = $state(settingString('carddav.schedule'));
  const uid = $props.id();
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
      provider: settingString('carddav.provider', source),
      oauthApp: settingString('carddav.oauth_app', source),
      baseURL: settingString('carddav.base_url', source),
      username: settingString('carddav.username', source),
      passwordConfigured: settingSecretConfigured('carddav.password', source),
      enabled: settingBoolean('carddav.enabled', source),
      schedule: settingString('carddav.schedule', source),
    };
  }
  function reconcileSettings(next: AccountSettingsSnapshot) {
    if (provider === persistedProvider) provider = next.provider;
    if (oauthApp === persistedOAuthApp) oauthApp = next.oauthApp;
    persistedProvider = next.provider;
    persistedOAuthApp = next.oauthApp;
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
      schedule,
    };
    if (google) { body.provider = 'google'; body.oauth_app = oauthApp; body.base_url = googleURL; }
    else if (password !== '') body.password = password;
    return body;
  }
  function identityTuple(): string {
    return `${provider}\u0000${oauthApp}\u0000${baseURL}\u0000${username}`;
  }
  function canReusePersistedPassword(): boolean {
    return provider === persistedProvider && oauthApp === persistedOAuthApp && persistedPasswordConfigured && baseURL === persistedBaseURL && username === persistedUsername;
  }
  // Only a stored account can be switched off without its password; a blank
  // form has nothing to disable.
  function canDisableWithoutPassword(): boolean {
    return provider === persistedProvider && oauthApp === persistedOAuthApp && !enabled && persistedBaseURL !== '' && baseURL === persistedBaseURL && username === persistedUsername;
  }
  function passwordRequiredForSave(): boolean {
    return !google && !canReusePersistedPassword() && !canDisableWithoutPassword();
  }
  function validatePassword(allowCredentialFreeDisable: boolean): boolean {
    if (google || canReusePersistedPassword() || password !== '' || (allowCredentialFreeDisable && canDisableWithoutPassword()))
      return true;
    status = '';
    error = 'Password is required for a new or changed CardDAV account.';
    return false;
  }
  async function connectGoogle() {
    if (activeAction !== undefined) return;
    if (!username.trim()) { error = 'Enter your Google account email first.'; return; }
    const controller = new AbortController();
    requestController = controller;
    const generation = ++actionGeneration;
    activeAction = 'authorize';
    error = '';
    status = '';
    try {
      await authorizeGoogleContacts(client, username, oauthApp, controller.signal);
      if (current(generation)) status = 'Google Contacts authorized. Test and save the connection to use it.';
    } catch (cause) {
      if (current(generation)) error = cause instanceof Error ? cause.message : 'Google sign-in failed.';
    } finally {
      if (current(generation)) { activeAction = undefined; requestController = undefined; }
    }
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
      const { data, error: responseError } = await generatedTestCardDAVAccount(requestBody(), {
        ...client,
        signal: controller.signal,
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
      const { data, error: responseError } = await generatedSaveCardDAVAccount(requestBody(), {
        ...client,
        signal: controller.signal,
      });
      if (!current(generation, controller.signal)) return;
      if (!data) throw new Error(apiErrorMessage(responseError, 'Unable to save the CardDAV account.'));
      const passwordConfigured = persistedPasswordConfigured || password !== '';
      baseURL = data.base_url;
      username = data.username;
      enabled = data.enabled;
      schedule = data.schedule ?? '';
      persistedProvider = provider;
      persistedOAuthApp = oauthApp;
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
      const message = (
        responseError as {
          message?: unknown;
        }
      ).message;
      if (typeof message === 'string' && message) return message;
    }
    return fallback;
  }
  function current(generation: number, signal: AbortSignal | undefined = undefined): boolean {
    return !disposed && generation === actionGeneration && !signal?.aborted;
  }
</script>

<SettingsSection title="CardDAV account" description="Address-book server this archive syncs contacts with.">
  {#if error}<p class="error" role="alert">{error}</p>{/if}
  {#if status}<p class="status" role="status">{status}</p>{/if}

  <p class="posture">
    <ZapIcon size={12} aria-hidden="true" />
    Saving the account applies right away. No daemon restart is needed.
  </p>
  <form
    onsubmit={(event) => {
      event.preventDefault();
      void saveAccount();
    }}
  >
    <label>
      Provider
      <SelectDropdown title="CardDAV provider" value={provider} options={[{ value: '', label: 'Other CardDAV server' }, { value: 'google', label: 'Google Contacts' }]} onchange={selectProvider} disabled={activeAction !== undefined} />
    </label>
    <div class="fields">
      {#if !google}
      <div class="field">
        <label class="field__label" for={`${uid}-url`}>Base URL</label>
        <TextInput id={`${uid}-url`} type="url" bind:value={baseURL} disabled={activeAction !== undefined} required block />
      </div>
      {/if}
      <div class="field">
        <label class="field__label" for={`${uid}-username`}>{google ? 'Google account email' : 'Username'}</label>
        <TextInput
          id={`${uid}-username`}
          autocomplete="username"
          bind:value={username}
          disabled={activeAction !== undefined}
          required
          block
        />
      </div>
    {#if google}
      <label>
        OAuth app
        <TextInput bind:value={oauthApp} disabled={activeAction !== undefined} placeholder="Default Google OAuth app" block />
      </label>
      <div class="actions">
        <Button label={activeAction === 'authorize' ? 'Waiting for Google…' : 'Connect Google'} disabled={activeAction !== undefined} onclick={() => void connectGoogle()} />
        {#if activeAction === 'authorize'}<Button label="Cancel sign-in" onclick={() => requestController?.abort()} />{/if}
      </div>
      <p>Register <code>{window.location.origin}/</code> as an authorized redirect URI in your Google OAuth app. Keep existing permissions checked when granting contacts access.</p>
      <p>Your contacts account and OAuth app can differ from your mail accounts. A matching Google authorization is reused; otherwise, CardDAV stores separate credentials.</p>
      <details><summary>Authorize from the terminal instead</summary>
        <code class="authorization-command">{authorizationCommand}</code>
        <p>For a remote daemon, copy the authorized token to that host before testing. <a href="https://msgvault.io/docs/usage/people-carddav/#google-contacts" target="_blank" rel="noreferrer">Google Contacts setup</a></p>
      </details>
    {:else}
      <div class="field">
        <label class="field__label" for={`${uid}-password`}>Password</label>
        <TextInput
          id={`${uid}-password`}
          type="password"
          autocomplete="current-password"
          bind:value={password}
          disabled={activeAction !== undefined}
          required={passwordRequiredForSave()}
          ariaDescribedby={`${uid}-password-hint`}
          block
        />
        <span class="field__hint" id={`${uid}-password-hint`}>
          {passwordRequiredForSave()
            ? 'Required for a new or changed account.'
            : canReusePersistedPassword()
              ? 'Leave blank to keep the stored password.'
              : 'Not needed to disable the account.'}
        </span>
      </div>
      {/if}
    </div>
    <div class="field">
      <div class="field__head">
        <span class="field__label">Automatic sync</span>
        <Toggle bind:checked={enabled} disabled={activeAction !== undefined} label="Enabled" />
      </div>
      <CronField label="Schedule" bind:value={schedule} disabled={activeAction !== undefined} />
    </div>

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
  .authorization-command { overflow-wrap: anywhere; user-select: all; }
  p { margin: 0; color: var(--text-secondary); }
  form {
    display: grid;
    gap: var(--space-5);
  }
  .posture {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    margin: 0 0 var(--space-5);
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }
  .posture :global(svg) {
    flex-shrink: 0;
    color: var(--accent-green);
  }
  .fields {
    display: grid;
    gap: var(--space-4);
  }
  .fields,
  .field {
    max-width: 26rem;
  }
  .field {
    display: grid;
    gap: var(--space-2);
  }
  .field__head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-3);
  }
  .field__label {
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
    font-weight: 500;
  }
  .field__hint {
    color: var(--text-muted);
    font-size: var(--font-size-2xs);
    line-height: 1.4;
  }
  .actions {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-3);
  }
  .error,
  .status {
    margin: 0;
    padding: var(--space-3) var(--space-4);
    border: 1px solid;
    border-radius: var(--radius-md);
  }
  .error {
    border-color: var(--status-error-ink);
    background: var(--status-error-bg);
    color: var(--status-error-ink);
  }
  .status {
    border-color: var(--status-success-ink);
    background: var(--status-success-bg);
    color: var(--status-success-ink);
  }
</style>
