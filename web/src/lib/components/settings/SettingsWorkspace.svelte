<script lang="ts" module>
  function optionLabel(value: string): string {
    const words = value.replaceAll('_', ' ');
    return words.charAt(0).toUpperCase() + words.slice(1);
  }
</script>

<script lang="ts">
  import {
    getSettings as generatedGetSettings,
    patchSettings as generatedPatchSettings,
  } from '../../api/generated/api/api';
  import {
    Button,
    SelectDropdown,
    SettingsLayout,
    SettingsSection,
    TextInput,
    Toggle,
    type SettingsCategory,
  } from '@kenn-io/kit-ui';
  import { onMount } from 'svelte';
  import type { APIClient } from '../../api/client';
  import type {
    PersonEnrichmentProviderSetting as GeneratedPersonEnrichmentProviderSetting,
    ProviderCredentialResponse as GeneratedProviderCredentialResponse,
    SettingUpdate as GeneratedSettingUpdate,
    SettingsResponse as GeneratedSettingsResponse,
  } from '../../api/generated/models';
  import type { CardDAVSettingsRequest } from '../../carddav/navigation';
  import CardDAVSettingsWorkspace from './CardDAVSettingsWorkspace.svelte';
  import PersonEnrichmentProviderCard from './PersonEnrichmentProviderCard.svelte';
  import PersonEnrichmentProviderCreator from './PersonEnrichmentProviderCreator.svelte';
  import ProviderCredentialControl from './ProviderCredentialControl.svelte';
  import {
    groupSettings,
    settingsCatalog,
    type SettingGroupState,
    type SettingState,
    type SettingValue,
  } from '../../settings/catalog';
  type SecretUpdate =
    | {
        action: 'set';
        value: string;
      }
    | {
        action: 'clear';
      };
  type SettingUpdate = GeneratedSettingUpdate;
  type ProviderSetting = GeneratedPersonEnrichmentProviderSetting;
  type CredentialResponse = GeneratedProviderCredentialResponse;
  type SettingsDocument = GeneratedSettingsResponse;
  let {
    client,
    plainHTTPWarning = false,
    cardDAVRequest = undefined,
    onCardDAVRequestConsumed = () => undefined,
  }: {
    client: APIClient;
    plainHTTPWarning?: boolean;
    cardDAVRequest?: CardDAVSettingsRequest;
    onCardDAVRequestConsumed?: (key: number) => void;
  } = $props();
  let settings = $state<SettingState[]>([]);
  let groups = $state<SettingGroupState[]>([]);
  let providers = $state<ProviderSetting[]>([]);
  let etag = $state('');
  let credentialETag = $state('');
  let drafts = $state<Record<string, unknown>>({});
  let secretUpdates = $state<Record<string, SecretUpdate>>({});
  let secretValues = $state<Record<string, string>>({});
  let pendingRestart = $state(false);
  let loading = $state(true);
  let saving = $state(false);
  let error = $state('');
  let activeCategory = $state('browser');
  let consumedCategoryRequestKey: number | undefined;
  const settingsGroups = $derived(groupSettings(settings, groups));
  const categories: SettingsCategory[] = $derived([
    ...settingsGroups.map((group) => ({
      id: group.id,
      label: group.label,
      summary: `${group.settings.length} ${group.settings.length === 1 ? 'setting' : 'settings'}`,
    })),
    {
      id: 'carddav',
      label: 'CardDAV account',
      summary: 'Address-book connection',
    },
  ]);
  onMount(() => {
    void loadSettings(false);
  });
  $effect(() => {
    const request = cardDAVRequest;
    if (!request || request.key === consumedCategoryRequestKey) return;
    activeCategory = 'carddav';
    if (request.conflictID === undefined) {
      consumedCategoryRequestKey = request.key;
      onCardDAVRequestConsumed(request.key);
    }
  });
  async function loadSettings(retainDrafts: boolean) {
    if (!retainDrafts) loading = true;
    try {
      const { data: document, error: responseError, response } = await generatedGetSettings(client);
      if (!document) throw new Error(apiErrorMessage(responseError, 'Unable to load settings.'));
      settings = document.settings;
      groups = document.groups ?? [];
      providers = document.person_enrichment_providers ?? [];
      pendingRestart = document.pending_restart;
      etag = response.headers.get('ETag') ?? '';
      credentialETag = response.headers.get('Credential-ETag') ?? document.credential_etag ?? '';
      if (!retainDrafts) {
        drafts = {};
        secretUpdates = {};
        secretValues = {};
      }
    } catch (cause) {
      error = cause instanceof Error ? cause.message : 'Unable to load settings.';
    } finally {
      loading = false;
    }
  }
  function currentValue(setting: SettingState): unknown {
    if (Object.hasOwn(drafts, setting.key)) return drafts[setting.key];
    const value = setting.value;
    if (!value) return undefined;
    if ('string' in value) return value.string;
    if ('integer' in value) return value.integer;
    if ('number' in value) return value.number;
    if ('boolean' in value) return value.boolean;
    return value.strings;
  }
  function setDraft(key: string, value: unknown) {
    drafts = { ...drafts, [key]: value };
  }
  function setSecret(key: string, value: string) {
    secretValues = { ...secretValues, [key]: value };
    if (value === '') {
      const next = { ...secretUpdates };
      delete next[key];
      secretUpdates = next;
      return;
    }
    secretUpdates = { ...secretUpdates, [key]: { action: 'set', value } };
  }
  function clearSecret(key: string) {
    secretValues = { ...secretValues, [key]: '' };
    secretUpdates = { ...secretUpdates, [key]: { action: 'clear' } };
  }
  async function saveSettings() {
    const updates: SettingUpdate[] = [
      ...Object.entries(drafts)
        .filter(([key]) => !settings.find((setting) => setting.key === key)?.read_only)
        .map(([key, value]) => ({
          key,
          value: typedValue(
            settings.find((setting) => setting.key === key),
            value,
          ),
        })),
      ...Object.entries(secretUpdates).map(([key, secret]) => ({ key, secret })),
    ];
    if (updates.length === 0) return;
    saving = true;
    error = '';
    try {
      const {
        data: result,
        error: responseError,
        response,
      } = await generatedPatchSettings(
        { updates },
        {
          ...client,
          headers: { 'If-Match': etag },
        },
      );
      if (response.status === 412) {
        await loadSettings(true);
        error =
          'The configuration changed on disk. Latest settings were loaded; review your local changes and save again.';
        return;
      }
      if (!result) {
        error = apiErrorMessage(responseError, 'Unable to save settings.');
        return;
      }
      settings = result.settings;
      groups = result.groups ?? groups;
      providers = result.person_enrichment_providers ?? providers;
      pendingRestart = result.pending_restart;
      etag = response.headers.get('ETag') ?? etag;
      credentialETag = response.headers.get('Credential-ETag') ?? result.credential_etag ?? credentialETag;
      drafts = {};
      secretUpdates = {};
      secretValues = {};
    } catch (cause) {
      error = cause instanceof Error ? cause.message : 'Unable to save settings.';
    } finally {
      saving = false;
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
  function stringValue(setting: SettingState): string {
    const value = currentValue(setting);
    if (Array.isArray(value)) return value.join(', ');
    return value == null ? '' : String(value);
  }
  function optionValues(setting: SettingState): string[] {
    return setting.options ?? settingsCatalog[setting.key]?.options ?? [];
  }
  function settingLabel(setting: SettingState): string {
    return setting.label || settingsCatalog[setting.key]?.label || humanizeKey(setting.key);
  }
  function settingDescription(setting: SettingState): string {
    return setting.description || settingsCatalog[setting.key]?.description || `Configures ${setting.key}.`;
  }
  function isReadOnly(setting: SettingState): boolean {
    return Boolean(setting.read_only) || hostManagedKeys.has(setting.key);
  }
  function credentialDisabledReason(credentialID: string): string {
    const endpointKey = credentialEndpointKeys[credentialID];
    if (endpointKey && Object.hasOwn(drafts, endpointKey)) {
      return 'Save endpoint settings first before changing this credential.';
    }
    return '';
  }
  function credentialSaved(response: CredentialResponse, nextETag: string) {
    credentialETag = nextETag;
    pendingRestart = pendingRestart || response.pending_restart;
    settings = settings.map((setting) =>
      setting.credential_id === response.credential_id ? { ...setting, secret: response.state } : setting,
    );
    providers = providers.map((provider) =>
      provider.credential_id === response.credential_id ? { ...provider, credential: response.state } : provider,
    );
  }
  async function credentialConflict() {
    await loadSettings(true);
  }
  function providerSaved(document: SettingsDocument, nextETag: string) {
    settings = document.settings;
    groups = document.groups ?? groups;
    providers = document.person_enrichment_providers ?? providers;
    pendingRestart = document.pending_restart;
    etag = nextETag;
    credentialETag = document.credential_etag || credentialETag;
  }
  async function providerConflict() {
    await loadSettings(true);
  }
  function typedValue(setting: SettingState | undefined, value: unknown): SettingValue {
    switch (setting?.kind) {
      case 'boolean':
        return { boolean: Boolean(value) };
      case 'integer':
        return { integer: Number(value) };
      case 'number':
        return { number: Number(value) };
      case 'string_array':
        return { strings: Array.isArray(value) ? value.map(String) : [] };
      default:
        return { string: String(value ?? '') };
    }
  }
  function sentenceLabel(label: string): string {
    return label.charAt(0).toLowerCase() + label.slice(1);
  }
  function humanizeKey(key: string): string {
    const tail = key.split('.').at(-1) ?? key;
    const words = tail.replaceAll('_', ' ');
    return words.charAt(0).toUpperCase() + words.slice(1);
  }
  const hostManagedKeys = new Set([
    'server.bind_addr',
    'server.api_port',
    'server.api_key',
    'server.allow_insecure',
    'server.trusted_proxies',
    'vector.backend',
    'vector.db_path',
    'vector.skip_extension_create',
    'vector.embeddings.api_key_env',
    'vector.multimodal.api_key_env',
    'vector.multimodal.capabilities_file',
  ]);
  const credentialEndpointKeys: Readonly<Record<string, string>> = {
    'vector.embeddings': 'vector.embeddings.endpoint',
    'vector.multimodal': 'vector.multimodal.endpoint',
  };
</script>

{#snippet settingsFooter()}
  <Button
    disabled={saving}
    tone="success"
    surface="solid"
    label={saving ? 'Saving…' : 'Save settings'}
    onclick={() => void saveSettings()}
  />
{/snippet}

<main class="settings" aria-label="Settings">
  <h1 class="kit-sr-only">Settings</h1>
  {#if loading}
    <p class="state" role="status">Loading settings…</p>
  {:else}
    <SettingsLayout
      {categories}
      bind:active={activeCategory}
      title="Settings"
      footer={activeCategory === 'carddav' ? undefined : settingsFooter}
    >
      {#snippet panel(activeId)}
        <div class="notices">
          {#if plainHTTPWarning}
            <p class="warning" role="alert">
              This browser session uses plain HTTP, so its cookie cannot use the Secure flag. Prefer HTTPS for remote
              access.
            </p>
          {/if}
          {#if error}<p class="error" role="alert">{error}</p>{/if}
          {#if pendingRestart}<p class="pending" role="status">Changes are pending restart.</p>{/if}
        </div>

        {#if activeId === 'carddav'}
          <CardDAVSettingsWorkspace
            {client}
            {settings}
            {cardDAVRequest}
            {onCardDAVRequestConsumed}
            onSettingsRefresh={() => loadSettings(true)}
          />
        {:else}
          {#each settingsGroups.filter((candidate) => candidate.id === activeId) as group (group.id)}
            <div class="settings-panel">
              <SettingsSection title={group.label} description={group.description}>
                {#each group.settings as setting (setting.key)}
                  {@const label = settingLabel(setting)}
                  <div class="field">
                    <div class="field-copy">
                      <strong>{label}</strong>
                      <span>{settingDescription(setting)}</span>
                      {#if setting.validation?.hint}<small>{setting.validation.hint}</small>{/if}
                      {#if setting.restart_required}<small>Restart required</small>{/if}
                    </div>

                    <div class="field-control">
                      {#if isReadOnly(setting)}
                        <div class="readonly-control">
                          <span>
                            {setting.kind === 'secret'
                              ? setting.secret?.configured
                                ? 'Configured'
                                : 'Not configured'
                              : stringValue(setting) || 'Not set'}
                          </span>
                          <small>Set via config.toml on the daemon host.</small>
                        </div>
                      {:else if setting.kind === 'secret' && setting.credential_id}
                        <ProviderCredentialControl
                          {client}
                          credentialID={setting.credential_id}
                          {label}
                          credentialState={setting.secret}
                          {credentialETag}
                          disabledReason={credentialDisabledReason(setting.credential_id)}
                          onSaved={credentialSaved}
                          onConflict={credentialConflict}
                        />
                      {:else if setting.kind === 'secret'}
                        <div class="secret-control">
                          <span>{setting.secret?.configured ? 'Set' : 'Not set'}</span>
                          <label>
                            New {sentenceLabel(label)}
                            <TextInput
                              type="password"
                              autocomplete="new-password"
                              value={secretValues[setting.key] ?? ''}
                              oninput={(value) => setSecret(setting.key, value)}
                              block
                            />
                          </label>
                          <Button label={`Clear ${sentenceLabel(label)}`} onclick={() => clearSecret(setting.key)} />
                        </div>
                      {:else if optionValues(setting).length > 0}
                        <label class="control-label">
                          <span class="kit-sr-only">{label}</span>
                          <SelectDropdown
                            value={stringValue(setting)}
                            title={label}
                            options={optionValues(setting).map((option) => ({
                              value: option,
                              label: optionLabel(option),
                            }))}
                            onchange={(value) => setDraft(setting.key, value)}
                          />
                        </label>
                      {:else if setting.kind === 'boolean'}
                        <Toggle
                          ariaLabel={label}
                          checked={Boolean(currentValue(setting))}
                          onchange={(checked) => setDraft(setting.key, checked)}
                        />
                      {:else if setting.kind === 'integer' || setting.kind === 'number'}
                        <label class="control-label">
                          <span class="kit-sr-only">{label}</span>
                          <input
                            type="number"
                            value={stringValue(setting)}
                            step={setting.kind === 'integer' ? '1' : 'any'}
                            min={setting.validation?.minimum}
                            max={setting.validation?.maximum}
                            required={setting.validation?.required}
                            oninput={(event) => setDraft(setting.key, Number(event.currentTarget.value))}
                          />
                        </label>
                      {:else}
                        <label class="control-label">
                          <span class="kit-sr-only">{label}</span>
                          <TextInput
                            value={stringValue(setting)}
                            block
                            oninput={(value) =>
                              setDraft(
                                setting.key,
                                setting.kind === 'string_array'
                                  ? value
                                      .split(',')
                                      .map((item) => item.trim())
                                      .filter(Boolean)
                                  : value,
                              )}
                          />
                        </label>
                      {/if}
                    </div>
                  </div>
                {/each}
                {#if group.id === 'enrichment'}
                  <div class="provider-list">
                    {#each ['exa', 'sixtyfour'] as kind}
                      <PersonEnrichmentProviderCreator
                        {client}
                        kind={kind as ProviderSetting['kind']}
                        existingNames={providers.map((provider) => provider.name)}
                        configETag={etag}
                        onSaved={providerSaved}
                        onConflict={providerConflict}
                      />
                    {/each}
                    {#each providers as provider (provider.name)}
                      <PersonEnrichmentProviderCard
                        {client}
                        {provider}
                        configETag={etag}
                        {credentialETag}
                        onSaved={providerSaved}
                        onConfigConflict={providerConflict}
                        onCredentialSaved={credentialSaved}
                        onCredentialConflict={credentialConflict}
                      />
                    {/each}
                  </div>
                {/if}
              </SettingsSection>
            </div>
          {/each}
        {/if}
      {/snippet}
    </SettingsLayout>
  {/if}
</main>

<style>
  .settings {
    display: flex;
    flex: 1;
    min-height: 0;
    width: 100%;
  }
  .settings :global(.kit-settings__nav-item--active),
  .settings :global(.kit-settings__nav-item--active:hover) {
    color: color-mix(in srgb, var(--accent-blue) 92%, var(--text-primary));
  }
  .state {
    padding: var(--space-6);
    color: var(--text-muted);
  }
  .field {
    display: grid;
    grid-template-columns: minmax(12rem, 1fr) minmax(14rem, 20rem);
    gap: var(--space-6);
    align-items: center;
    padding-block: var(--space-3);
  }
  .field + .field {
    border-top: 1px solid var(--border-muted);
  }
  .field-copy {
    display: grid;
    gap: 0.25rem;
  }
  .field-copy span,
  small {
    color: var(--text-muted);
  }
  .field-control {
    display: flex;
    align-items: center;
    justify-content: flex-end;
    gap: var(--space-3);
    min-width: 0;
  }
  .control-label,
  .secret-control,
  .readonly-control {
    width: 100%;
    min-width: 0;
  }
  input[type='number'] {
    width: 100%;
    min-height: 2.25rem;
  }
  .secret-control {
    display: grid;
    gap: 0.5rem;
  }
  .provider-list {
    display: grid;
    gap: var(--space-4);
    margin-top: var(--space-4);
  }
  .readonly-control {
    display: grid;
    gap: 0.25rem;
  }
  .readonly-control small {
    color: var(--text-muted);
  }
  .notices:empty {
    display: none;
  }
  .notices {
    display: grid;
    gap: var(--space-3);
  }
  .notices p {
    margin: 0;
  }
  .warning,
  .pending {
    padding: 0.75rem 1rem;
    border: 1px solid var(--status-warning-ink);
    border-radius: var(--radius-md);
    background: var(--status-warning-bg);
    color: var(--status-warning-ink);
  }
  .error {
    padding: 0.75rem 1rem;
    border: 1px solid var(--status-error-ink);
    border-radius: var(--radius-md);
    background: var(--status-error-bg);
    color: var(--status-error-ink);
  }
  :global(.confirmation) {
    margin-right: auto;
  }

  @media (max-width: 640px) {
    .field {
      grid-template-columns: 1fr;
      gap: var(--space-3);
    }
    .field-control {
      justify-content: flex-start;
      flex-wrap: wrap;
    }
  }
</style>
