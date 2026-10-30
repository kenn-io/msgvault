<script lang="ts">
  import {
    getEmbeddingStatus as generatedGetEmbeddingStatus,
    getOperationStatus as generatedGetOperationStatus,
    getSettings as generatedGetSettings,
    getVisualAttachmentStatus as generatedGetVisualAttachmentStatus,
    patchSettings as generatedPatchSettings,
  } from '../../api/generated/api/api';
  import { Notice, SettingsLayout, Toggle, Button, IconButton, EmptyState, type SettingsCategory } from '@kenn-io/kit-ui';
  import { getContext, onMount, onDestroy, tick, untrack } from 'svelte';
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
  import { runOperationAction } from '../../operations/actions';
  import { pendingSecret } from '../../settings/secrets';
  import type { SearchCoverageValue } from '../../search/modes';
  import {
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
  const inference = new PeopleInferenceController(untrack(() => client), () => loadSettings(true));
  onDestroy(() => inference.destroy());
  let pendingRestart = $state(false);
  let loading = $state(true);
  let loaded = $state(false);
  let saving = $state(false);
  let error = $state('');
  let buildError = $state('');
  let activeCategory = $state(untrack(() => category));
  let root = $state<HTMLElement>();
  let embeddingStatus = $state<EmbeddingStatus>();
  let visualStatus = $state<VisualStatus>();
  let lanes = $state<OperationLaneStatus[]>();
  let coverage = $state<Pick<SearchCoverageValue, 'status' | 'actions'>>();
  let statusSettled = $state(false);
  let building = $state<Record<string, boolean>>({});
  let consumedCategoryRequestKey: number | undefined;
  let focusedNavigationSettingKey: string | undefined;

  const categories: SettingsCategory[] = RAIL.map(({ id, label }) => ({ id, label }));
  const label = $derived(categories.find((candidate) => candidate.id === activeCategory)?.label ?? '');
  const layout = $derived(layoutCategory(activeCategory as CategoryID, settings, groups));
  const dirtyCount = $derived(Object.keys(drafts).length + Object.keys(secretUpdates).length);
  const draftNeedsRestart = $derived(settings.some(setting => setting.restart_required && isDirty(setting.key)));
  let pendingLeave = $state<{ leave: () => void; stay?: () => void }>();
  const registerLeave = getContext<((hold?: (leave: () => void, stay?: () => void) => boolean) => void) | undefined>('msgvault:hold-settings-leave');
  registerLeave?.((leave, stay) => dirtyCount > 0 && !saving && (pendingLeave = { leave, stay }, true));
  onDestroy(() => registerLeave?.(undefined));
  function focusStay(node: HTMLElement) {
    node.querySelector('button')?.focus();
  }
  function stay() {
    pendingLeave?.stay?.();
    pendingLeave = undefined;
  }
  function discardAndLeave() {
    const leave = pendingLeave?.leave;
    pendingLeave = undefined;
    discardChanges();
    leave?.();
  }
  // An emptied number field is a draft in progress, not a value: it keeps the
  // row on, cannot be saved, and never stands in for the off value.
  const incompleteDrafts = $derived(Object.entries(drafts).filter(([key, value]) => isIncompleteNumber(find(key), value)).length);
  const features = $derived(overviewRows(lanes ?? [], embeddingStatus, visualStatus, coverage));

  onMount(() => {
    void loadSettings(false);
  });

  let followedCategory = untrack(() => category);
  $effect(() => {
    if (category === followedCategory) return;
    followedCategory = category;
    activeCategory = category;
  });
  // Any navigation while a leave is pending cancels it. A rail click stays
  // first, so an unindexed held entry gets Settings back before the push.
  $effect(() => {
    void [category, navigationTarget];
    untrack(() => { pendingLeave = undefined; });
  });
  function selectCategory(categoryID: string): void {
    stay();
    onCategoryChange(activeCategory = categoryID);
  }

  // Overview owns feature status and index actions. Status reloads when a build starts or ends.
  let statusRevision = $state(0);
  $effect(() => {
    if (activeCategory !== 'overview') return;
    void [statusRevision, Object.keys(building)];
    const controller = new AbortController();
    void createExploreAPI(client).coverage([], controller.signal)
      .catch(() => coverage ?? { status: 'unavailable' as const, actions: [] })
      .then(value => { if (!controller.signal.aborted) coverage = value; });
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
      });
    return () => controller.abort();
  });

  async function build(id: string, run: () => Promise<void>): Promise<void> {
    if (building[id]) return;
    building[id] = true;
    buildError = '';
    try {
      await run();
    } catch (cause) {
      buildError = cause instanceof Error ? cause.message : "Can't build the index.";
    } finally {
      delete building[id];
    }
  }

  async function focusSetting(categoryID: string | undefined, settingKey: string | undefined): Promise<void> {
    if (categoryID) selectCategory(categoryID);
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
      const result = await runOperationAction(action.id, client);
      if (result.outcome !== 'succeeded') throw new Error(result.message);
    });
  }

  $effect(() => {
    const target = navigationTarget;
    if (target) {
      activeCategory = target.categoryID;
      if (loading || target.settingKey === focusedNavigationSettingKey) return;
      void focusSetting(undefined, target.settingKey);
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
      error = '';
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
    drafts = Object.fromEntries(Object.entries(drafts).filter(([key, value]) => isChange(find(key), value)));
    secretUpdates = Object.fromEntries(Object.entries(secretUpdates).filter(([key, update]) => update.action === 'set' || find(key)?.secret?.configured));
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
  }
  // The save bar takes the focused button with it, so focus moves to the
  // category heading.
  async function focusCategoryHeading() {
    await tick();
    root?.querySelector<HTMLElement>('#settings-category-heading')?.focus();
  }
  function discard() {
    discardChanges();
    void focusCategoryHeading();
  }
  function isDirty(key: string): boolean {
    return key in drafts || key in secretUpdates;
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

  async function saveSettings() {
    if (saving || inference.busy || dirtyCount === 0) return;
    const updates: SettingUpdate[] = [
      ...Object.entries(drafts)
        .filter(([key]) => !find(key)?.read_only)
        .map(([key, value]) => ({ key, value: typedValue(find(key), value) })),
      ...Object.entries(secretUpdates).map(([key, secret]) => ({ key, secret })),
    ];
    saving = true;
    error = '';
    try {
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
      discardChanges();
      void focusCategoryHeading();
      const appearance = savedAppearance(result.settings, new Set(updates.map(({ key }) => key)));
      if (appearance) onAppearanceSaved(appearance);
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
  function credentialDisabledReason(credentialID: string): string {
    return Object.hasOwn(drafts, `${credentialID}.endpoint`) ? 'Save endpoint settings first before changing this credential.' : '';
  }
  function providerSaved(document: SettingsDocument, nextETag: string) {
    settings = document.settings;
    providers = document.person_enrichment_providers ?? providers;
    pendingRestart = document.pending_restart;
    etag = nextETag;
    inference.updateConfigRevision(etag);
    credentialETag = document.credential_etag || credentialETag;
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
    const off = setting.validation?.off;
    if (off && value === (off.value ?? '')) return off.label;
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

<svelte:window onbeforeunload={(event) => { if (dirtyCount > 0) event.preventDefault(); }} />

{#snippet settingsFooter()}
  {#if pendingLeave}
    <span class="unsaved" role="status">Leave Settings and discard {dirtyCount} unsaved {dirtyCount === 1 ? 'change' : 'changes'}?</span>
    <span use:focusStay>
      <Button label="Stay" tone="neutral" surface="outline" onclick={stay} />
    </span>
    <Button label="Discard and leave" tone="info" surface="solid" onclick={discardAndLeave} />
  {:else}
    <span class="unsaved" role="status">
      {`${dirtyCount} unsaved ${dirtyCount === 1 ? 'change' : 'changes'}${incompleteDrafts > 0 ? '. Enter a number to save.' : draftNeedsRestart ? '.' : ''}`}
      {#if draftNeedsRestart}<span>Restart msgvault to apply these changes.</span>{/if}
    </span>
    <Button label="Discard" disabled={saving} onclick={discard} />
    <Button
      disabled={saving || inference.busy || incompleteDrafts > 0}
      tone="info"
      surface="solid"
      label={saving ? 'Saving…' : 'Save changes'}
      onclick={() => void saveSettings()}
    />
  {/if}
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
            <ProviderCredentialControl
              {client}
              credentialID={setting.credential_id}
              label={text}
              credentialState={setting.secret}
              {credentialETag}
              disabledReason={credentialDisabledReason(setting.credential_id)}
              restartRequired={Boolean(setting.restart_required)}
              onSaved={credentialSaved}
              onConflict={() => loadSettings(true)}
            />
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
    {#if isDirty(setting.key)}<span class="kit-sr-only"> (unsaved)</span>{/if}
  </div>
{/snippet}

{#snippet blocks(list: readonly Block[])}
  {#each list as block, index (index)}
    {#if block.description || block.rows.some(row => rowFields(row).length > 0)}
    <section class="block" aria-label={block.title}>
      {#if block.title}
        <div class="block__head">
          <h3>{block.title}</h3>
          {#if block.description}<p class="setting__hint">{block.description}</p>{/if}
        </div>
      {/if}
      <div class="rows">
      {#each block.rows as row, rowIndex (rowIndex)}
        {@const fields = rowFields(row)}
        {#if fields.length > 0}
          <div class="row">
            <div class="fields" class:fields--pair={fields.length > 1}>
              {#each fields as setting (setting.key)}{@render control(setting)}{/each}
            </div>
            <!-- An untitled block's description belongs to the switch that leads it. -->
            {#if rowIndex === 0 && !block.title && block.description}<p class="setting__hint">{block.description}</p>{/if}
          </div>
        {/if}
      {/each}
      </div>
    </section>
    {/if}
  {/each}
{/snippet}

<main class="settings" bind:this={root} aria-label="Settings">
  <div class="settings-header">
    <PageHeader title="Settings">
      {#snippet actions()}
        {#if activeCategory === 'overview'}
          <span class="feature-refresh"><IconButton ariaLabel="Refresh feature status" onclick={() => (statusRevision += 1)}><RefreshCwIcon size={16} /></IconButton></span>
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
      footer={pendingLeave || dirtyCount > 0 ? settingsFooter : undefined}
    >
      {#snippet panel(activeId)}
        <div class="notices">
          {#if plainHTTPWarning && activeId === 'server'}
            <Notice tone="warning" message="This connection uses plain HTTP. Use HTTPS for remote access." />
          {/if}
          {#if error}<p class="notice notice--error" role="alert">{error}</p>{/if}
          {#if buildError && activeId === 'overview'}<p class="notice notice--error" role="alert">{buildError}</p>{/if}
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
            {client}
            {settings}
            {cardDAVRequest}
            {onCardDAVRequestConsumed}
            onSettingsRefresh={() => loadSettings(true)}
          />
        {:else}
          {@render blocks(layout.blocks)}

          {#if activeId === 'enrichment'}
            <section class="block" aria-label="Providers">
              <h3>Providers</h3>
              <div class="provider-list">
                {#each providers as provider (provider.name)}
                  <PersonEnrichmentProviderCard
                    {client}
                    {provider}
                    configETag={etag}
                    {credentialETag}
                    onSaved={providerSaved}
                    onConfigConflict={() => loadSettings(true)}
                    onCredentialSaved={credentialSaved}
                    onCredentialConflict={() => loadSettings(true)}
                  />
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

          {#if layout.advanced.length > 0 || layout.config.length > 0}
          <div class="disclosures">
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
          </div>
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
    gap: var(--space-7);
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
    line-height: 1.45;
  }
  .settings-header {
    --page-header-gap: 0px;
    padding: var(--space-5) var(--page-gutter) var(--field-gap);
  }
  .settings :global(.kit-settings) {
    background: var(--bg-surface);
  }
  /* Reserve the scrollbar's width up front so the centered panel does not
     shift left when a long category makes it scroll. Positioning contains the
     rows' absolutely positioned screen-reader labels, which otherwise stretch
     the document past the viewport and add a second scrollbar. */
  .settings :global(.kit-settings__scroll) {
    position: relative;
    scrollbar-gutter: stable;
  }
  .settings :global(.kit-settings__panel),
  .settings :global(.kit-settings__footer) {
    padding: var(--pane-pad-y) var(--pane-pad-x);
  }
  @media (max-width: 760px) {
    /* Title, category tabs, and fields share one left edge. */
    .settings-header {
      padding-inline: var(--pane-pad-x);
    }
    .settings :global(.kit-settings__sidebar) {
      padding-inline: calc(var(--pane-pad-x) - var(--space-4));
    }
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
    line-height: 1.45;
  }
  .notice--error {
    border-color: var(--status-error-ink);
    background: var(--status-error-bg);
    color: var(--status-error-ink);
  }

  h2 {
    margin: 0;
    line-height: 1.2;
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
    gap: var(--space-5);
    min-width: 0;
  }
  .block__head,
  .row {
    display: grid;
    gap: var(--control-gap);
    min-width: 0;
  }
  .rows,
  .fields {
    display: grid;
    gap: var(--section-gap);
    min-width: 0;
  }
  .fields--pair {
    grid-template-columns: repeat(auto-fit, minmax(14rem, 1fr));
    gap: var(--section-gap);
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
    gap: var(--control-gap);
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
  }
  .setting__label {
    color: var(--text-secondary);
    font-size: var(--font-size-xs);
  }
  .setting--switch .setting__label {
    /* Centering the line box leaves the letters looking high beside the switch. */
    position: relative;
    top: 1px;
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
    margin: 0;
    color: var(--text-muted);
    font-size: var(--font-size-2xs);
    line-height: 1.45;
  }

  .disclosures {
    display: grid;
    gap: var(--field-gap);
  }
  .disclosure[open] {
    padding-bottom: var(--space-5);
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
    line-height: 1.45;
  }
</style>
