<script lang="ts" module>
  const optionLabels: Readonly<Record<string, string>> = {
    openai: 'OpenAI-compatible',
    'voyage-contextual': 'Voyage contextual',
    duckdb: 'DuckDB',
    sql: 'SQL',
    'sqlite-vec': 'sqlite-vec',
    pgvector: 'pgvector',
  };

  function optionLabel(value: string): string {
    if (Object.hasOwn(optionLabels, value)) return optionLabels[value];
    if (value === '') return 'Default';
    const words = value.replaceAll('_', ' ');
    return words.charAt(0).toUpperCase() + words.slice(1);
  }

  const hostManagedNote = 'Host-managed values are set in config.toml on the daemon host.';
</script>

<script lang="ts">
  import {
    getSettings as generatedGetSettings,
    patchSettings as generatedPatchSettings,
  } from '../../api/generated/api/api';
  import {
    Button,
    Card,
    Chip,
    SelectDropdown,
    SettingsLayout,
    SettingsSection,
    TextInput,
    Toggle,
    type SettingsCategory,
  } from '@kenn-io/kit-ui';
  import { onMount, tick } from 'svelte';
  import type { APIClient } from '../../api/client';
  import type {
    PersonEnrichmentProviderSetting as GeneratedPersonEnrichmentProviderSetting,
    ProviderCredentialResponse as GeneratedProviderCredentialResponse,
    SettingUpdate as GeneratedSettingUpdate,
    SettingsResponse as GeneratedSettingsResponse,
  } from '../../api/generated/models';
  import type { CardDAVSettingsRequest, SettingsNavigationTarget } from '../../carddav/navigation';
  import CardDAVSettingsWorkspace from './CardDAVSettingsWorkspace.svelte';
  import PersonEnrichmentProviderCard from './PersonEnrichmentProviderCard.svelte';
  import PersonEnrichmentProviderCreator from './PersonEnrichmentProviderCreator.svelte';
  import ProviderCredentialControl from './ProviderCredentialControl.svelte';
  import {
    groupSettings,
    hasHostManaged,
    restartPosture,
    type RestartPosture,
    type SettingGroupState,
    type SettingState,
    type SettingValue,
    type SettingsGroup,
    type SettingsSection as SettingsSectionState,
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
  type ControlSize = 'xs' | 'md' | 'lg';
  let {
    client,
    plainHTTPWarning = false,
    cardDAVRequest = undefined,
    navigationTarget = undefined,
    onCardDAVRequestConsumed = () => undefined,
  }: {
    client: APIClient;
    plainHTTPWarning?: boolean;
    cardDAVRequest?: CardDAVSettingsRequest;
    navigationTarget?: SettingsNavigationTarget;
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
  let root = $state<HTMLElement>();
  let consumedCategoryRequestKey: number | undefined;
  let focusedNavigationSettingKey: string | undefined;
  const settingsGroups = $derived(groupSettings(settings, groups));
  const categories: SettingsCategory[] = $derived([
    ...settingsGroups.map((group) => ({ id: group.id, label: group.label })),
    { id: 'carddav', label: 'CardDAV account' },
  ]);
  const dirtyCount = $derived(Object.keys(drafts).length + Object.keys(secretUpdates).length);
  onMount(() => {
    void loadSettings(false);
  });
  $effect(() => {
    const target = navigationTarget;
    if (target) {
      activeCategory = target.categoryID;
      if (loading) return;
      if (target.settingKey === focusedNavigationSettingKey) return;
      const settingKey = target.settingKey;
      void tick().then(() => root?.querySelector<HTMLElement>(
        `[data-setting-key="${settingKey}"]`
      )?.focus());
      focusedNavigationSettingKey = settingKey;
      return;
    }
    if (focusedNavigationSettingKey !== undefined) {
      focusedNavigationSettingKey = undefined;
      activeCategory = 'browser';
    }
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
      if (!retainDrafts) discardChanges();
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
  function discardChanges() {
    drafts = {};
    secretUpdates = {};
    secretValues = {};
  }
  function isDirty(key: string): boolean {
    return Object.hasOwn(drafts, key) || Object.hasOwn(secretUpdates, key);
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
      discardChanges();
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
    return setting.options ?? [];
  }
  function settingLabel(setting: SettingState): string {
    return setting.label || humanizeKey(setting.key);
  }
  function controlSize(setting: SettingState): ControlSize {
    if (setting.kind === 'integer' || setting.kind === 'number') return 'xs';
    if (optionValues(setting).length > 0) return 'md';
    return 'lg';
  }
  function isReadOnly(setting: SettingState): boolean {
    return Boolean(setting.read_only) || hostManagedKeys.has(setting.key);
  }
  function readOnlyValue(setting: SettingState): string {
    if (setting.kind === 'secret') return setting.secret?.configured ? 'Configured' : 'Not configured';
    return stringValue(setting) || 'Not set';
  }
  function postureText(posture: RestartPosture): string {
    switch (posture) {
      case 'live':
        return 'Changes apply right away.';
      case 'restart':
        return 'Changes take effect after the daemon restarts.';
      case 'mixed':
        return 'Most changes take effect after the daemon restarts. Rows that differ are marked.';
      default:
        return 'Set in config.toml on the daemon host.';
    }
  }
  // In a category where most rows share one posture, only the exceptions
  // carry a chip. Every current category is uniform, so this stays quiet.
  function rowFlag(setting: SettingState, group: SettingsGroup): string {
    if (isReadOnly(setting) || restartPosture(group.settings) !== 'mixed') return '';
    const editable = group.settings.filter((candidate) => !candidate.read_only);
    const needsRestart = editable.filter((candidate) => candidate.restart_required).length;
    const mostNeedRestart = needsRestart * 2 >= editable.length;
    if (mostNeedRestart) return setting.restart_required ? '' : 'Applies right away';
    return setting.restart_required ? 'Needs restart' : '';
  }
  function sectionDescription(section: SettingsSectionState): string {
    return [section.description, hasHostManaged(section.settings) ? hostManagedNote : '']
      .filter(Boolean)
      .join(' ');
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
  <span class="unsaved" role="status">
    {dirtyCount === 0 ? 'No unsaved changes' : `${dirtyCount} unsaved ${dirtyCount === 1 ? 'change' : 'changes'}`}
  </span>
  <Button label="Discard" disabled={saving || dirtyCount === 0} onclick={discardChanges} />
  <Button
    disabled={saving || dirtyCount === 0}
    tone="success"
    surface="solid"
    label={saving ? 'Saving…' : 'Save settings'}
    onclick={() => void saveSettings()}
  />
{/snippet}

{#snippet row(setting: SettingState, group: SettingsGroup)}
  {@const label = settingLabel(setting)}
  {@const flag = rowFlag(setting, group)}
  {@const readOnly = isReadOnly(setting)}
  {@const hint = readOnly ? '' : (setting.validation?.hint ?? '')}
  <div
    class="row"
    class:row--changed={isDirty(setting.key)}
    class:row--host={readOnly}
    data-setting-key={setting.key}
    tabindex="-1"
  >
    <div class="row__text">
      <span class="row__label">
        {label}{#if isDirty(setting.key)}<span class="kit-sr-only"> (unsaved)</span>{/if}
      </span>
      {#if setting.description}<span class="row__hint">{setting.description}</span>{/if}
    </div>

    <div class="row__control">
      <div class="row__widgets">
        {#if readOnly}
          {@const value = readOnlyValue(setting)}
          {#if setting.kind === 'secret' || value === 'Not set'}
            <span class="row__value" class:row__value--unset={value === 'Not set'}>{value}</span>
          {:else}
            <span class="row__value" data-mono>{value}</span>
          {/if}
          <Chip size="xs" tone="muted">Host-managed</Chip>
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
            <span class="row__value" class:row__value--unset={!setting.secret?.configured}>
              {setting.secret?.configured ? 'Set' : 'Not set'}
            </span>
            <label class="secret-control__field">
              <span class="kit-sr-only">New {sentenceLabel(label)}</span>
              <TextInput
                type="password"
                autocomplete="new-password"
                placeholder={`New ${sentenceLabel(label)}`}
                value={secretValues[setting.key] ?? ''}
                oninput={(value) => setSecret(setting.key, value)}
                block
              />
            </label>
            <Button label={`Clear ${sentenceLabel(label)}`} onclick={() => clearSecret(setting.key)} />
          </div>
        {:else if optionValues(setting).length > 0}
          <label class="row__field" data-size="md">
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
          <label class="row__field" data-size="xs">
            <span class="kit-sr-only">{label}</span>
            <input
              type="number"
              data-mono
              value={stringValue(setting)}
              step={setting.kind === 'integer' ? '1' : 'any'}
              min={setting.validation?.minimum}
              max={setting.validation?.maximum}
              required={setting.validation?.required}
              oninput={(event) => setDraft(setting.key, Number(event.currentTarget.value))}
            />
          </label>
        {:else}
          <label class="row__field" data-size="lg">
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
        {#if flag}
          <Chip size="xs" tone={flag === 'Needs restart' ? 'warning' : 'info'}>{flag}</Chip>
        {/if}
      </div>
      {#if hint}<small class="row__format">{hint}</small>{/if}
    </div>
  </div>
{/snippet}

<main class="settings" bind:this={root} aria-label="Settings">
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
            <p class="notice notice--warning" role="alert">
              This browser session uses plain HTTP, so its cookie cannot use the Secure flag. Prefer HTTPS for remote
              access.
            </p>
          {/if}
          {#if error}<p class="notice notice--error" role="alert">{error}</p>{/if}
          {#if pendingRestart}
            <p class="notice notice--pending" role="status">
              <strong>Saved.</strong> Restart the daemon to apply these changes.
            </p>
          {/if}
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
            {@const posture = restartPosture(group.settings)}
            <header class="category">
              <h2>{group.label}</h2>
              {#if group.description}<p>{group.description}</p>{/if}
              <p class="posture" data-posture={posture}>{postureText(posture)}</p>
            </header>

            {#if group.sections.length > 0}
              {#each group.sections as section (section.id)}
                <SettingsSection title={section.label} description={sectionDescription(section) || undefined}>
                  {#each section.settings as setting (setting.key)}
                    {@render row(setting, group)}
                  {/each}
                </SettingsSection>
              {/each}
            {:else}
              <Card padding="md">
                <div class="rows">
                  {#each group.settings as setting (setting.key)}
                    {@render row(setting, group)}
                  {/each}
                </div>
              </Card>
            {/if}

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
  /* Section titles sit one step above row labels so the two levels read apart. */
  .settings :global(.kit-settings-section__title) {
    font-size: var(--font-size-lg);
  }
  .state {
    padding: var(--space-6);
    color: var(--text-muted);
  }

  .notices:empty {
    display: none;
  }
  .notices {
    display: grid;
    gap: var(--space-3);
  }
  .notice {
    margin: 0;
    padding: 0.75rem 1rem;
    border: 1px solid var(--status-warning-ink);
    border-radius: var(--radius-md);
    background: var(--status-warning-bg);
    color: var(--status-warning-ink);
    font-size: var(--font-size-sm);
  }
  .notice strong {
    font-weight: 650;
  }
  .notice--error {
    border-color: var(--status-error-ink);
    background: var(--status-error-bg);
    color: var(--status-error-ink);
  }

  .category {
    display: grid;
    gap: var(--space-1);
  }
  .category h2 {
    margin: 0;
    font-size: var(--font-size-xl);
    font-weight: 650;
    letter-spacing: -0.01em;
  }
  .category p {
    margin: 0;
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
    line-height: 1.45;
  }
  .category .posture {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    margin-top: var(--space-2);
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }
  .posture::before {
    content: '';
    box-sizing: border-box;
    width: 10px;
    height: 10px;
    border: 1.5px solid currentColor;
    border-radius: 50%;
  }
  .posture[data-posture='live']::before {
    border-color: var(--accent-green);
    background: var(--accent-green);
  }
  .posture[data-posture='restart']::before,
  .posture[data-posture='mixed']::before {
    border-right-color: transparent;
  }

  .rows {
    display: flex;
    flex-direction: column;
    gap: var(--space-5);
  }
  .row {
    display: grid;
    grid-template-columns: minmax(0, 1fr) auto;
    gap: var(--space-6);
    align-items: center;
    border-radius: var(--radius-sm);
  }
  .row:focus-visible {
    outline: var(--focus-ring);
    outline-offset: 6px;
  }
  .row__text {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    min-width: 0;
  }
  .row__label {
    color: var(--text-primary);
    font-size: var(--font-size-sm);
    font-weight: 500;
  }
  .row--host .row__label {
    color: var(--text-secondary);
  }
  .row--changed .row__label::after {
    content: '';
    display: inline-block;
    width: 6px;
    height: 6px;
    margin-left: 8px;
    border-radius: 50%;
    background: var(--accent-amber);
    vertical-align: middle;
  }
  .row__hint {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    line-height: 1.45;
  }
  .row__control {
    display: grid;
    justify-items: end;
    gap: var(--space-1);
    max-width: 24rem;
  }
  .row__widgets {
    display: flex;
    align-items: center;
    justify-content: flex-end;
    gap: var(--space-3);
    min-width: 0;
  }
  .row__field {
    display: block;
    min-width: 0;
  }
  .row__field[data-size='xs'] {
    width: 6.5rem;
  }
  .row__field[data-size='md'] {
    width: 13rem;
  }
  .row__field[data-size='lg'] {
    width: 20rem;
  }
  .row__format {
    color: var(--text-muted);
    font-size: var(--font-size-2xs);
    line-height: 1.4;
    text-align: right;
  }
  .row__value {
    color: var(--text-primary);
    font-size: var(--font-size-sm);
  }
  .row__value--unset {
    color: var(--text-muted);
  }
  input[type='number'] {
    box-sizing: border-box;
    width: 100%;
    height: 28px;
    padding: 0 var(--space-3);
    border: var(--border-width) solid var(--border-default);
    border-radius: var(--radius-md);
    background: var(--bg-surface);
    color: var(--text-primary);
    font: inherit;
    font-size: var(--font-size-sm);
    transition: border-color var(--transition-fast) var(--transition-ease, ease);
  }
  input[type='number']:focus-visible {
    outline: var(--focus-ring);
    outline-offset: 1px;
    border-color: var(--accent-blue);
  }
  .secret-control {
    display: flex;
    align-items: center;
    gap: var(--space-3);
  }
  .secret-control__field {
    display: block;
    width: 14rem;
  }
  .provider-list {
    display: grid;
    gap: var(--space-4);
  }
  .unsaved {
    margin-right: auto;
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }
  :global(.confirmation) {
    margin-right: auto;
  }

  @media (max-width: 640px) {
    .row {
      grid-template-columns: 1fr;
      gap: var(--space-3);
    }
    .row__control {
      justify-items: start;
      max-width: none;
    }
    .row__widgets {
      justify-content: flex-start;
      flex-wrap: wrap;
    }
    .row__format {
      text-align: left;
    }
    .row__field[data-size],
    .secret-control__field {
      width: 100%;
      max-width: 20rem;
    }
    .secret-control {
      flex-wrap: wrap;
    }
  }
</style>
