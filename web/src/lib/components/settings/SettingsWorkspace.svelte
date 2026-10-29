<script lang="ts">
  import {
    deleteSettingsProviderCredential as generatedDeleteSettingsProviderCredential,
    getEmbeddingStatus as generatedGetEmbeddingStatus,
    getOperationStatus as generatedGetOperationStatus,
    getSettings as generatedGetSettings,
    getVisualAttachmentStatus as generatedGetVisualAttachmentStatus,
    patchSettings as generatedPatchSettings,
    putSettingsProviderCredential as generatedPutSettingsProviderCredential,
    saveCardDAVAccount,
    putSettingsPersonEnrichmentProvider,
  } from '../../api/generated/api/api';
  import { Notice, SettingsLayout, Toggle, Button, IconButton, EmptyState, type SettingsCategory } from '@kenn-io/kit-ui';
  import { onMount, onDestroy, tick, untrack } from 'svelte';
  import type { APIClient } from '../../api/client';
  import type {
    EmbeddingStatus,
    PersonEnrichmentProviderSetting as GeneratedPersonEnrichmentProviderSetting,
    ProviderCredentialResponse as GeneratedProviderCredentialResponse,
    SettingUpdate as GeneratedSettingUpdate,
    SettingsResponse as GeneratedSettingsResponse,
    OperationLaneStatus,
    Status as VisualStatus,
  } from '../../api/generated/models';
  import { createExploreAPI } from '../../explore/api';
  import { settingsNavigationTarget, type CardDAVSettingsRequest, type SettingsNavigationTarget } from '../../carddav/navigation';
  import type { SavedAppearance } from '../../theme/preferences.svelte';
  import { PeopleInferenceController } from '../../settings/people-inference-controller.svelte';
  import { accountRequest, type AccountDraft } from '../../carddav/account-settings';
  import { runOperationAction } from '../../operations/actions';
  import { pendingSecret } from '../../settings/secrets';
  import type { SearchCoverageAction } from '../../search/modes';
  import {
    providerUpdate,
    isChange,
    isIncompleteNumber,
    persistedValue,
    typedValue,
    valueText,
    type SettingState,
    type SettingGroupState,
  } from '../../settings/catalog';
  import { overviewRows, type OverviewAction } from '../../settings/overview';
  import {
    LABELS,
    OPTION_LABELS,
    RAIL,
    layoutCategory,
    type Block,
    type CategoryID,
  } from '../../settings/layout';
  import RefreshCwIcon from '@lucide/svelte/icons/refresh-cw';
  import PageHeader from '../shell/PageHeader.svelte';
  import CardDAVSettingsWorkspace from './CardDAVSettingsWorkspace.svelte';
  import PeopleInferenceSettings from './PeopleInferenceSettings.svelte';
  import PersonEnrichmentProviderCard from './PersonEnrichmentProviderCard.svelte';
  import PersonEnrichmentProviderCreator from './PersonEnrichmentProviderCreator.svelte';
  import SecretField from './SecretField.svelte';
  import ProviderCredentialControl from './ProviderCredentialControl.svelte';
  import SettingField from './SettingField.svelte';
  import SettingsOverview from './SettingsOverview.svelte';

  type SecretUpdate = { action: 'set'; value: string } | { action: 'clear' };
  type SettingUpdate = GeneratedSettingUpdate;
  type ProviderSetting = GeneratedPersonEnrichmentProviderSetting;
  type CredentialResponse = GeneratedProviderCredentialResponse;
  type SettingsDocument = GeneratedSettingsResponse;

  let {
    client,
    plainHTTPWarning = false,
    cardDAVRequest = undefined,
    navigationTarget = undefined,
    category = 'overview',
    onCategoryChange = () => undefined,
    onSources = () => undefined,
    onCardDAVRequestConsumed = () => undefined,
    onAppearanceSaved = () => undefined,
  }: {
    client: APIClient;
    plainHTTPWarning?: boolean;
    cardDAVRequest?: CardDAVSettingsRequest;
    navigationTarget?: SettingsNavigationTarget;
    category?: string;
    onSources?: () => void;
    onCategoryChange?: (categoryID: string) => void;
    onCardDAVRequestConsumed?: (key: number) => void;
    onAppearanceSaved?: (saved: SavedAppearance) => void;
  } = $props();


  let settings = $state<SettingState[]>([]);
  let groups = $state<SettingGroupState[]>([]);
  let providers = $state<ProviderSetting[]>([]);
  let etag = $state('');
  let credentialETag = $state('');
  let drafts = $state<Record<string, unknown>>({});
  let secretUpdates = $state<Record<string, SecretUpdate>>({});
  // Keys for provider credentials wait here, keyed by credential id, until Save changes.
  let credentialUpdates = $state<Record<string, SecretUpdate>>({});
  let providerDrafts = $state<Record<string, ProviderSetting>>({});
  let accountDrafts = $state<Record<string, AccountDraft>>({});
  const inference = new PeopleInferenceController(untrack(() => client), () => loadSettings(true));
  onDestroy(() => inference.destroy());
  let pendingRestart = $state(false);
  let loading = $state(true);
  let loaded = $state(false);
  let saving = $state(false);
  let error = $state('');
  let accountSaveStatus = $state('');
  let activeCategory = $state(untrack(() => category));
  let root = $state<HTMLElement>();
  let embeddingStatus = $state<EmbeddingStatus>();
  let visualStatus = $state<VisualStatus>();
  let lanes = $state<OperationLaneStatus[]>();
  let coverageActions = $state<SearchCoverageAction[]>([]);
  let statusSettled = $state(false);
  let statusRefreshing = $state(false);
  let building = $state('');
  let consumedCategoryRequestKey: number | undefined;
  let focusedNavigationSettingKey: string | undefined;

  const categories: SettingsCategory[] = RAIL.map(({ id, label }) => ({ id, label }));
  const label = $derived(categories.find((candidate) => candidate.id === activeCategory)?.label ?? '');
  const layout = $derived(layoutCategory(activeCategory as CategoryID, settings, groups));
  const dirtyCount = $derived(
    Object.keys(drafts).length +
      Object.keys(secretUpdates).length +
      Object.keys(credentialUpdates).length +
      Object.keys(providerDrafts).length + Object.keys(accountDrafts).length + Object.keys(inference.keyDrafts).length,
  );
  // An emptied number field is a draft in progress, not a value: it keeps the
  // row on, cannot be saved, and never stands in for the off value.
  const incompleteDrafts = $derived(
    Object.entries(drafts).filter(([key, value]) => isIncompleteNumber(find(key), value)).length +
    Object.values(providerDrafts).filter(provider => [provider.max_retries, provider.max_requests_per_run, provider.max_requests_per_day, provider.num_results].some(value => String(value) === '')).length,
  );
  const features = $derived(overviewRows(lanes ?? [], embeddingStatus, visualStatus, coverageActions));

  onMount(() => {
    void loadSettings(false);
  });

  // A parent re-render that leaves the category unchanged must not undo a
  // category chosen or requested locally.
  let followedCategory = untrack(() => category);
  $effect(() => {
    if (category === followedCategory) return;
    followedCategory = category;
    activeCategory = category;
  });
  function selectCategory(categoryID: string): void {
    activeCategory = categoryID;
    onCategoryChange(categoryID);
  }

  // Overview owns feature status and index actions.
  let statusRevision = $state(0);
  $effect(() => {
    if (activeCategory !== 'overview') return;
    void statusRevision;
    const controller = new AbortController();
    const control = untrack(() => root?.querySelector<HTMLElement>('.feature-refresh'));
    const hadFocus = control?.contains(document.activeElement);
    statusRefreshing = true;
    coverageActions = [];
    void createExploreAPI(client).coverage([], controller.signal)
      .then(coverage => { if (!controller.signal.aborted) coverageActions = coverage.actions; })
      .catch(() => undefined);
    void generatedGetEmbeddingStatus(undefined, { ...client, signal: controller.signal })
      .then(({ data, response }) => {
        if (!controller.signal.aborted) embeddingStatus = response.ok ? data : undefined;
      })
      .catch(() => undefined);
    void generatedGetVisualAttachmentStatus({ ...client, signal: controller.signal })
      .then(({ data, response }) => {
        if (!controller.signal.aborted) visualStatus = response.ok ? data : undefined;
      })
      .catch(() => undefined);
    void generatedGetOperationStatus({ ...client, signal: controller.signal })
      .then(({ data, response }) => {
        if (controller.signal.aborted) return;
        lanes = response.ok ? data?.lanes : undefined;
        statusSettled = true;
      })
      .catch(() => {
        if (!controller.signal.aborted) { lanes = undefined; statusSettled = true; }
      })
      .finally(async () => {
        if (controller.signal.aborted) return;
        statusRefreshing = false;
        await tick();
        const active = document.activeElement;
        if (hadFocus && (!active || active === document.body || control?.contains(active))) control?.querySelector<HTMLButtonElement>('button')?.focus();
      });
    return () => controller.abort();
  });

  async function build(id: string, run: () => Promise<void>): Promise<void> {
    if (building) return;
    building = id;
    error = '';
    try {
      await run();
    } catch (cause) {
      error = cause instanceof Error ? cause.message : "Can't build the index.";
    } finally {
      building = '';
      statusRevision += 1;

    }
  }

  async function focusSetting(categoryID: string, settingKey: string | undefined): Promise<void> {
    selectCategory(categoryID);
    await tick();
    const field = settingKey
      ? root?.querySelector<HTMLElement>(`[data-setting-key="${settingKey}"]`)
      : root?.querySelector<HTMLElement>('#settings-category-heading');
    field?.closest('details')?.setAttribute('open', '');
    field?.focus();
  }
  function setUp(action: Extract<OverviewAction, { kind: 'settings' }>): void {
    const target = settingsNavigationTarget(action.target.settingsAuthority);
    void focusSetting(target?.categoryID ?? action.target.settingsCategory, target?.settingKey);
  }
  function startFeature(action: Extract<OverviewAction, { kind: 'start' }>): void {
    void build(action.id, async () => {
      if (action.id === 'messages') return createExploreAPI(client).runCoverageAction('build_index', 'initializing');
      const result = await runOperationAction(action.id === 'contacts' ? 'carddav_sync' : action.id, client);
      if (result.outcome !== 'succeeded') throw new Error(result.message);
    });
  }

  $effect(() => {
    const target = navigationTarget;
    if (target) {
      activeCategory = target.categoryID;
      if (loading || target.settingKey === focusedNavigationSettingKey) return;
      void focusSetting(target.categoryID, target.settingKey);
      focusedNavigationSettingKey = target.settingKey;
      return;
    }
    focusedNavigationSettingKey = undefined;
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
      if (!document) throw new Error(apiErrorMessage(responseError, "Can't load settings."));
      loaded = true;
      settings = document.settings;
      groups = document.groups;
      providers = document.person_enrichment_providers ?? [];
      pendingRestart = document.pending_restart;
      etag = response.headers.get('ETag') ?? '';
      inference.updateConfigRevision(etag);
      credentialETag = response.headers.get('Credential-ETag') ?? document.credential_etag ?? '';
      if (retainDrafts) pruneSettledDrafts();
      else discardChanges();
    } catch (cause) {
      error = cause instanceof Error ? cause.message : "Can't load settings.";
    } finally {
      loading = false;
    }
  }
  function find(key: string): SettingState | undefined {
    return settings.find((candidate) => candidate.key === key);
  }
  function currentValue(setting: SettingState): unknown {
    return Object.hasOwn(drafts, setting.key) ? drafts[setting.key] : persistedValue(setting);
  }
  // A draft that equals the persisted value is not a change: the row stays
  // clean, Save stays disabled, and no no-op PATCH can mark a restart pending.
  function setDraft(key: string, value: unknown) {
    const setting = find(key);
    if (setting && !isChange(setting, value)) {
      const next = { ...drafts };
      delete next[key];
      drafts = next;
      return;
    }
    drafts = { ...drafts, [key]: value };
  }
  // After a reload that keeps local edits, drop the ones the daemon already
  // holds: a draft equal to the new persisted value, or a clear for a secret
  // that is no longer configured.
  function pruneSettledDrafts() {
    const nextDrafts = { ...drafts };
    for (const key of Object.keys(nextDrafts)) {
      if (!isChange(find(key), nextDrafts[key])) delete nextDrafts[key];
    }
    drafts = nextDrafts;
    const nextSecrets = { ...secretUpdates };
    for (const [key, update] of Object.entries(nextSecrets)) {
      if (update.action === 'clear' && !find(key)?.secret?.configured) delete nextSecrets[key];
    }
    secretUpdates = nextSecrets;
  }
  function setCredential(id: string, update: SecretUpdate | undefined) {
    if (update) credentialUpdates[id] = update;
    else delete credentialUpdates[id];
  }
  function setSecret(setting: SettingState, value: string) {
    secretUpdates = { ...secretUpdates, [setting.key]: { action: 'set', value } };
  }
  // Clearing drops a key that is still waiting, and stages the removal of a stored one.
  function clearSecret(setting: SettingState) {
    const configured = setting.secret?.configured;
    const next = { ...secretUpdates };
    delete next[setting.key];
    if (configured) next[setting.key] = { action: 'clear' };
    secretUpdates = next;
  }
  function discardChanges() {
    drafts = {};
    secretUpdates = {};
    credentialUpdates = {};
    providerDrafts = {};
    accountDrafts = {};
    inference.keyDrafts = {};
  }
  // The save bar takes the focused button with it, so focus moves to the
  // category heading.
  async function focusCategoryHeading() {
    await tick();
    root?.querySelector<HTMLElement>('#settings-category-heading')?.focus();
  }
  function discard() {
    discardChanges();
    resetKey += 1;
    void focusCategoryHeading();
  }
  let resetKey = $state(0);
  function isDirty(key: string): boolean {
    const credentialID = find(key)?.credential_id;
    return key in drafts || key in secretUpdates || Boolean(credentialID && credentialUpdates[credentialID]);
  }

  const APPEARANCE_FIELDS: Readonly<Record<string, keyof SavedAppearance>> = {
    'web.theme': 'theme',
    'web.density': 'density',
    'web.default_search_mode': 'defaultSearchMode',
  };
  function savedAppearance(saved: readonly SettingState[], keys: ReadonlySet<string>): SavedAppearance | undefined {
    const result: SavedAppearance = {};
    for (const setting of saved) {
      const field = Object.hasOwn(APPEARANCE_FIELDS, setting.key) ? APPEARANCE_FIELDS[setting.key] : undefined;
      const value = setting.value && 'string' in setting.value ? setting.value.string : undefined;
      if (field && keys.has(setting.key) && typeof value === 'string') result[field] = value;
    }
    return Object.keys(result).length > 0 ? result : undefined;
  }

  // Endpoints save before credentials, which bind to their saved destination.
  async function saveSettings() {
    if (saving || inference.busy || dirtyCount === 0) return;
    const updates: SettingUpdate[] = [
      ...Object.entries(drafts)
        .filter(([key]) => !find(key)?.read_only)
        .map(([key, value]) => ({ key, value: typedValue(find(key), value) })),
      ...Object.entries(secretUpdates).map(([key, secret]) => ({ key, secret })),
    ];
    async function saveProviders(enabled: boolean) {
      for (const [name, draft] of Object.entries(providerDrafts)) {
        if (draft.enabled !== enabled) continue;
        const result = await putSettingsPersonEnrichmentProvider({ name }, providerUpdate(draft), { ...client, headers: { 'If-Match': etag } });
        if (!result.data) {
          if (result.response.status === 412) await loadSettings(true);
          throw new Error(apiErrorMessage(result.error, "Can't save this provider."));
        }
        providerSaved(result.data, result.response.headers.get('ETag') ?? etag);
        delete providerDrafts[name];
      }
    }
    saving = true;
    error = '';
    accountSaveStatus = '';
    try {
      for (const draft of Object.values(accountDrafts)) {
        if (draft.error) throw new Error(draft.error);
      }
      await saveProviders(true);
      if (updates.length > 0) {
        const {
          data: result,
          error: responseError,
          response,
        } = await generatedPatchSettings({ updates }, { ...client, headers: { 'If-Match': etag } });
        if (response.status === 412) {
          await loadSettings(true);
          error = 'The settings changed on disk. The latest are loaded. Review your changes and save again.';
          if (dirtyCount === 0) void focusCategoryHeading();
          return;
        }
        if (!result) {
          error = apiErrorMessage(responseError, "Can't save settings.");
          return;
        }
        settings = result.settings;
        providers = result.person_enrichment_providers ?? providers;
        pendingRestart = result.pending_restart;
        etag = response.headers.get('ETag') ?? etag;
        inference.updateConfigRevision(etag);
        credentialETag = response.headers.get('Credential-ETag') ?? result.credential_etag ?? credentialETag;
        drafts = {};
        secretUpdates = {};
        const appearance = savedAppearance(result.settings, new Set(updates.map(({ key }) => key)));
        if (appearance) onAppearanceSaved(appearance);

      }
      await saveProviders(false);
      for (const [credentialID, update] of Object.entries(credentialUpdates)) {
        const options = { ...client, headers: { 'If-Match': credentialETag } };
        const {
          data,
          error: responseError,
          response,
        } = update.action === 'set'
          ? await generatedPutSettingsProviderCredential({ credentialId: credentialID }, { value: update.value }, options)
          : await generatedDeleteSettingsProviderCredential({ credentialId: credentialID }, options);
        if (response.status === 412) {
          await loadSettings(true);
          error = 'The key changed elsewhere. The latest is loaded. Review your changes and save again.';
          return;
        }
        if (!data) {
          error = apiErrorMessage(responseError, "Can't save the key.");
          return;
        }
        credentialSaved(data, response.headers.get('ETag') ?? credentialETag);
        const next = { ...credentialUpdates };
        delete next[credentialID];
        credentialUpdates = next;
      }
      for (const [name, draft] of Object.entries(accountDrafts)) {
        const result = await saveCardDAVAccount(accountRequest(draft.values, draft.password, name), client);
        if (!result.data) throw new Error(apiErrorMessage(result.error, "Can't save the contact account."));
        accountSaveStatus = `Contact account ${name} saved. Found ${result.data.books} address ${result.data.books === 1 ? 'book' : 'books'}.`;
        await loadSettings(true);
        delete accountDrafts[name];
        resetKey += 1;
      }
      if (!(await inference.saveKeys())) {
        const message = inference.error;
        inference.error = '';
        throw new Error(message);
      }
      void focusCategoryHeading();
    } catch (cause) {
      error = cause instanceof Error ? cause.message : "Can't save settings.";
    } finally {
      statusRevision += 1;
      saving = false;
    }
  }
  function apiErrorMessage(responseError: unknown, fallback: string): string {
    if (typeof responseError === 'object' && responseError !== null && 'message' in responseError) {
      const message = (responseError as { message?: unknown }).message;
      if (typeof message === 'string' && message) return message;
    }
    return fallback;
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
  function providerSaved(document: SettingsDocument, nextETag: string) {
    settings = document.settings;
    providers = document.person_enrichment_providers ?? providers;
    pendingRestart = document.pending_restart;
    etag = nextETag;
    inference.updateConfigRevision(etag);
    credentialETag = document.credential_etag || credentialETag;

  }
  function setProviderDraft(value: ProviderSetting) {
    const saved = providers.find(provider => provider.name === value.name);
    if (saved && JSON.stringify(providerUpdate(saved)) === JSON.stringify(providerUpdate(value))) delete providerDrafts[value.name];
    else providerDrafts = { ...providerDrafts, [value.name]: value };
  }
  function rowLabel(setting: SettingState): string {
    if (Object.hasOwn(LABELS, setting.key)) return LABELS[setting.key]!;
    return setting.label || humanizeKey(setting.key);
  }
  function humanizeKey(key: string): string {
    const words = (key.split('.').at(-1) ?? key).replaceAll('_', ' ');
    return words.charAt(0).toUpperCase() + words.slice(1);
  }
  function readOnlyValue(setting: SettingState): string {
    if (setting.kind === 'secret') return setting.secret?.configured ? setting.secret.hint || 'Set' : 'None';
    const value = valueText(persistedValue(setting));
    return value || 'Not set';
  }
  // Rows that only matter once something else is on stay out of the way until then.
  function visible(setting: SettingState): boolean {

    if (setting.key.startsWith('vector.people.') && setting.key !== 'vector.people.enabled') {
      return isDirty(setting.key) || Boolean(currentValue(find('vector.people.enabled') ?? setting));
    }
    return true;
  }
  function rowFields(row: string | readonly string[]): SettingState[] {
    return (typeof row === 'string' ? [row] : [...row])
      .map(find)
      .filter((setting): setting is SettingState => setting !== undefined && !setting.read_only && visible(setting));
  }
  function isSwitch(setting: SettingState): boolean {
    return setting.kind === 'boolean' && !(setting.options && setting.options.length > 0);
  }
</script>

{#snippet settingsFooter()}
  <span class="unsaved" role="status">
    {`${dirtyCount} unsaved ${dirtyCount === 1 ? 'change' : 'changes'}${incompleteDrafts > 0 ? '. Enter a number to save.' : ''}`}
  </span>
  <Button label="Discard" disabled={saving} onclick={discard} />
  <Button
    disabled={saving || inference.busy || incompleteDrafts > 0}
    tone="info"
    surface="solid"
    label={saving ? 'Saving…' : 'Save changes'}
    onclick={() => void saveSettings()}
  />
{/snippet}

<!-- One setting: a switch, a key, or a labelled field. -->
{#snippet control(setting: SettingState)}
  {@const text = rowLabel(setting)}
  {@const on = Boolean(currentValue(setting))}
  <div
    class="setting"
    class:setting--switch={isSwitch(setting)}
    class:setting--changed={isDirty(setting.key)}
    data-setting-key={setting.key}
    tabindex="-1"
  >
    {#if isSwitch(setting)}
      <Toggle ariaLabel={text} checked={on} onchange={(checked) => setDraft(setting.key, checked)} />
      <span class="setting__label">{text}</span>

    {:else}
      <span class="setting__label">{text}</span>
      <div class="setting__control">
        {#if setting.kind === 'secret'}
          {@const shown = pendingSecret(setting.secret, secretUpdates[setting.key])}
          {#if setting.credential_id}
            <ProviderCredentialControl label={text} credentialState={setting.secret} pending={credentialUpdates[setting.credential_id]} onchange={(update) => setCredential(setting.credential_id!, update)} />
          {:else}
          <SecretField
            label={text}
            configured={shown.configured}
            hint={shown.hint}
            source={setting.secret?.source}
            disabled={setting.secret?.source === 'environment'}
            onreplace={(value) => {
              setSecret(setting, value);
              return true;
            }}
            onclear={shown.configured
              ? () => clearSecret(setting)
              : undefined}
          />
          {/if}
        {:else}
          <SettingField
            {setting}
            label={text}
            value={currentValue(setting)}
            optionLabels={OPTION_LABELS[setting.key]}
            onchange={(value) => setDraft(setting.key, value)}
          />
        {/if}
      </div>
      {#if setting.validation?.hint}<small class="setting__hint">{setting.validation.hint}</small>{/if}
    {/if}
  </div>
{/snippet}

{#snippet blocks(list: readonly Block[])}
  {#each list as block, index (index)}
    {#if block.description || block.rows.some(row => rowFields(row).length > 0)}
    <section class="block" aria-label={block.title}>
      {#if block.title}<h3>{block.title}</h3>{/if}
      {#if block.description}<p class="setting__hint">{block.description}</p>{/if}
      {#each block.rows as row, rowIndex (rowIndex)}
        {@const fields = rowFields(row)}
        {#if fields.length > 0}
          <div class="fields" class:fields--pair={fields.length > 1}>
            {#each fields as setting (setting.key)}{@render control(setting)}{/each}
          </div>
        {/if}
      {/each}
    </section>
    {/if}
  {/each}
{/snippet}

<main class="settings" bind:this={root} aria-label="Settings">
  <div class="settings-header">
    <PageHeader title="Settings">
      {#snippet actions()}
        {#if activeCategory === 'overview'}
          <span class="feature-refresh"><IconButton ariaLabel="Refresh feature status" disabled={statusRefreshing} onclick={() => (statusRevision += 1)}><RefreshCwIcon size={16} /></IconButton></span>
        {/if}
      {/snippet}
    </PageHeader>
  </div>
  {#if loading}
    <p class="state" role="status">Loading…</p>
  {:else if !loaded}
    <EmptyState title="Can't load settings"><Button label="Retry" onclick={() => void loadSettings(false)} /></EmptyState>
  {:else}
    <SettingsLayout
      {categories}
      bind:active={() => activeCategory, selectCategory}
      title=""
      footer={dirtyCount === 0 ? undefined : settingsFooter}
    >
      {#snippet panel(activeId)}
        <div class="notices">
          {#if plainHTTPWarning && activeId === 'server'}
            <Notice tone="warning" message="This connection uses plain HTTP. Use HTTPS for remote access." />
          {/if}
          {#if error}<p class="notice notice--error" role="alert">{error}</p>{/if}
          {#if accountSaveStatus}<p class="notice" role="status">{accountSaveStatus}</p>{/if}
          {#if pendingRestart}
            <p class="notice notice--pending" role="status">Saved. Restart msgvault to apply these changes.</p>
          {/if}
        </div>

        <h2 id="settings-category-heading" tabindex="-1">{label}</h2>

        <fieldset class="settings-fields" disabled={saving}>
        {#if activeId === 'overview'}
          {#if !statusSettled}
            <p class="state" role="status">Loading…</p>
          {:else if !lanes}
            <EmptyState title="Can't load status"><Button label="Retry" onclick={() => (statusRevision += 1)} /></EmptyState>
          {:else}
            <SettingsOverview {onSources} rows={features} working={building} onSetUp={(action) => void setUp(action)} onStart={startFeature} />
          {/if}
        {:else if activeId === 'carddav'}
          <CardDAVSettingsWorkspace
            resetRevision={resetKey}
            {client}
            {settings}
            bind:drafts={accountDrafts}
            {cardDAVRequest}
            {onCardDAVRequestConsumed}
          />
        {:else}
          {@render blocks(layout.blocks)}

          {#if activeId === 'enrichment'}
            <section class="block" aria-label="Providers">
              <h3>Providers</h3>
              <div class="provider-list">
                {#each providers as provider (provider.name)}
                  <PersonEnrichmentProviderCard {provider} draft={(Object.hasOwn(providerDrafts, provider.name) ? providerDrafts[provider.name] : undefined) ?? provider} pendingKey={credentialUpdates[provider.credential_id]} onchange={setProviderDraft} onkey={(update) => setCredential(provider.credential_id, update)} />
                {/each}
                <div class="provider-add">
                  {#each ['exa', 'sixtyfour'] as kind (kind)}
                    <PersonEnrichmentProviderCreator
                      {client}
                      kind={kind as ProviderSetting['kind']}
                      existingNames={providers.map((provider) => provider.name)}
                      configETag={etag}
                      onSaved={providerSaved}
                      onConflict={() => loadSettings(true)}
                    />
                  {/each}
                </div>
              </div>
            </section>
            <PeopleInferenceSettings {client} controller={inference} />
          {/if}

          {#if layout.advanced.length > 0}
            <details class="disclosure">
              <summary>Advanced</summary>
              <div class="fields fields--list">
                {#each layout.advanced as setting (setting.key)}{@render control(setting)}{/each}
              </div>
            </details>
          {/if}

          {#if layout.config.length > 0}
            <details class="disclosure">
              <summary>Set in config.toml</summary>
              <dl class="config">
                {#each layout.config as setting (setting.key)}
                  <div data-setting-key={setting.key}>
                    <dt>{rowLabel(setting)}</dt>
                    <dd data-mono>{readOnlyValue(setting)}</dd>
                  </div>
                {/each}
              </dl>
            </details>
          {/if}
        {/if}
        </fieldset>
      {/snippet}
    </SettingsLayout>
  {/if}
</main>

<style>
  .settings-fields {
    display: flex;
    flex-direction: column;
    gap: var(--space-6);
    min-width: 0;
    margin: 0;
    padding: 0;
    border: 0;
  }
  .settings {
    display: flex;
    flex: 1;
    flex-direction: column;
    min-height: 0;
    width: 100%;
    background: var(--bg-surface);
  }
  .settings-header {
    --page-header-gap: 0px;
    padding: var(--space-5) var(--page-gutter) var(--field-gap);
  }
  .settings :global(.kit-settings) {
    background: var(--bg-surface);
  }
  .settings :global(.kit-settings__panel),
  .settings :global(.kit-settings__footer) {
    padding: var(--pane-pad-y) var(--pane-pad-x);
  }
  .settings :global(.kit-settings__nav-item--active),
  .settings :global(.kit-settings__nav-item--active:hover) {
    color: color-mix(in srgb, var(--accent-blue) 92%, var(--text-primary));
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
    gap: var(--field-gap);
  }
  .notices :global(.kit-notice) {
    padding: var(--pane-pad-y) var(--pane-pad-x);
    gap: var(--control-gap);
  }
  .notice {
    margin: 0;
    padding: var(--pane-pad-y) var(--pane-pad-x);
    border: 1px solid var(--status-warning-ink);
    border-radius: var(--radius-md);
    background: var(--status-warning-bg);
    color: var(--status-warning-ink);
    font-size: var(--font-size-sm);
  }
  .notice--error {
    border-color: var(--status-error-ink);
    background: var(--status-error-bg);
    color: var(--status-error-ink);
  }

  h2 {
    margin: 0;
    font-size: var(--font-size-xl);
    font-weight: var(--font-weight-semibold);
    letter-spacing: -0.01em;
  }
  h3 {
    margin: 0;
    font-size: var(--font-size-md);
    font-weight: var(--font-weight-semibold);
  }

  .block {
    display: grid;
    gap: var(--field-gap);
    min-width: 0;
  }
  .fields {
    display: grid;
    gap: var(--field-gap);
    min-width: 0;
  }
  .fields--pair {
    grid-template-columns: repeat(auto-fit, minmax(14rem, 1fr));
    gap: var(--field-gap) var(--section-gap);
    align-items: start;
  }
  .fields--list {
    gap: var(--section-gap);
    margin-top: var(--field-gap);
  }

  /* A switch sits first, then its label and status. Other settings put the
     label above the field. */
  .setting {
    display: grid;
    gap: var(--space-2);
    min-width: 0;
    border-radius: var(--radius-sm);
  }
  .setting:focus-visible {
    outline: var(--focus-ring);
    outline-offset: 4px;
  }
  .setting--switch {
    grid-template-columns: auto 1fr;
    align-items: center;
    justify-content: start;
    gap: var(--control-gap);
    min-height: var(--control-height);
  }
  .setting__label {
    color: var(--text-secondary);
    font-size: var(--font-size-xs);
  }
  .setting--switch .setting__label {
    color: var(--text-primary);
    font-size: var(--font-size-sm);
  }
  .setting--changed .setting__label::after {
    content: '';
    display: inline-block;
    width: 6px;
    height: 6px;
    margin-left: var(--space-4);
    border-radius: 50%;
    background: var(--accent-amber);
    vertical-align: middle;
  }
  .setting__control {
    display: flex;
    align-items: center;
    gap: var(--control-gap);
    min-width: 0;
  }
  .setting__hint {
    color: var(--text-muted);
    font-size: var(--font-size-2xs);
    line-height: var(--line-height-body);
  }

  .disclosure summary {
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
    cursor: pointer;
  }
  .config {
    display: grid;
    gap: var(--field-gap);
    margin: var(--field-gap) 0 0;
  }
  .config div {
    display: grid;
    grid-template-columns: minmax(10rem, 14rem) minmax(0, 1fr);
    gap: var(--section-gap);
    font-size: var(--font-size-sm);
  }
  .config dt {
    color: var(--text-secondary);
  }
  .config dd {
    margin: 0;
    overflow-wrap: anywhere;
    color: var(--text-primary);
  }
  .provider-list {
    display: grid;
    gap: var(--field-gap);
  }
  .provider-add {
    display: flex;
    flex-wrap: wrap;
    gap: var(--control-gap);
  }
  .unsaved {
    margin-right: auto;
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }
</style>
