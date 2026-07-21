<script lang="ts">
  import { onMount } from 'svelte';

  import type { APIClient } from '../../api/client';
  import type { components } from '../../api/generated/schema';
  import {
    groupSettings,
    settingsCatalog,
    type SettingState,
    type SettingValue
  } from '../../settings/catalog';

  type SecretUpdate = { action: 'set'; value: string } | { action: 'clear' };
  type SettingUpdate = components['schemas']['SettingUpdate'];

  let {
    client,
    plainHTTPWarning = false,
    onTestConnection
  }: {
    client: APIClient;
    plainHTTPWarning?: boolean;
    onTestConnection?: (key: string) => void | Promise<void>;
  } = $props();

  let settings = $state<SettingState[]>([]);
  let etag = $state('');
  let drafts = $state<Record<string, unknown>>({});
  let secretUpdates = $state<Record<string, SecretUpdate>>({});
  let secretValues = $state<Record<string, string>>({});
  let pendingRestart = $state(false);
  let loading = $state(true);
  let saving = $state(false);
  let error = $state('');
  let confirmAPIKeyRestart = $state(false);

  onMount(() => {
    void loadSettings(false);
  });

  async function loadSettings(retainDrafts: boolean) {
    loading = true;
    try {
      const { data: document, error: responseError, response } = await client.GET('/api/v1/settings');
      if (!document) throw new Error(apiErrorMessage(responseError, 'Unable to load settings.'));
      settings = document.settings;
      pendingRestart = document.pending_restart;
      etag = response.headers.get('ETag') ?? '';
      if (!retainDrafts) {
        drafts = {};
        secretUpdates = {};
        secretValues = {};
        confirmAPIKeyRestart = false;
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
      ...Object.entries(drafts).map(([key, value]) => ({
        key,
        value: typedValue(settings.find((setting) => setting.key === key), value)
      })),
      ...Object.entries(secretUpdates).map(([key, secret]) => ({ key, secret }))
    ];
    if (updates.length === 0) return;
    if (Object.hasOwn(secretUpdates, 'server.api_key') && !confirmAPIKeyRestart) {
      error = 'You must confirm that the API key changes after restart.';
      return;
    }

    saving = true;
    error = '';
    try {
      const { data: result, error: responseError, response } = await client.PATCH('/api/v1/settings', {
        params: { header: { 'If-Match': etag } },
        body: {
          updates,
          confirm_api_key_restart: confirmAPIKeyRestart
        }
      });
      if (response.status === 412) {
        await loadSettings(true);
        error = 'The configuration changed on disk. Latest settings were loaded; review your local changes and save again.';
        return;
      }
      if (!result) {
        error = apiErrorMessage(responseError, 'Unable to save settings.');
        return;
      }

      settings = result.settings;
      pendingRestart = result.pending_restart;
      etag = response.headers.get('ETag') ?? etag;
      drafts = {};
      secretUpdates = {};
      secretValues = {};
      confirmAPIKeyRestart = false;
    } catch (cause) {
      error = cause instanceof Error ? cause.message : 'Unable to save settings.';
    } finally {
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

  function stringValue(setting: SettingState): string {
    const value = currentValue(setting);
    if (Array.isArray(value)) return value.join(', ');
    return value == null ? '' : String(value);
  }

  function optionValues(setting: SettingState): string[] {
    return setting.options ?? settingsCatalog[setting.key]?.options ?? [];
  }

  function typedValue(setting: SettingState | undefined, value: unknown): SettingValue {
    switch (setting?.kind) {
      case 'boolean': return { boolean: Boolean(value) };
      case 'integer': return { integer: Number(value) };
      case 'number': return { number: Number(value) };
      case 'string_array': return { strings: Array.isArray(value) ? value.map(String) : [] };
      default: return { string: String(value ?? '') };
    }
  }

  function sentenceLabel(label: string): string {
    return label.charAt(0).toLowerCase() + label.slice(1);
  }
</script>

<main class="settings" aria-label="Settings">
  <header>
    <p class="eyebrow">msgvault</p>
    <h1>Settings</h1>
    <p>Changes are written to config.toml and use optimistic concurrency.</p>
  </header>

  {#if plainHTTPWarning}
    <p class="warning" role="alert">
      This browser session uses plain HTTP, so its cookie cannot use the Secure flag. Prefer HTTPS for remote access.
    </p>
  {/if}

  {#if error}
    <p class="error" role="alert">{error}</p>
  {/if}

  {#if pendingRestart}
    <p class="pending" role="status">Changes are pending restart.</p>
  {/if}

  {#if loading}
    <p role="status">Loading settings…</p>
  {:else}
    <form onsubmit={(event) => { event.preventDefault(); void saveSettings(); }}>
      {#each groupSettings(settings) as group (group.id)}
        <section aria-labelledby={`settings-${group.id}`}>
          <h2 id={`settings-${group.id}`}>{group.label}</h2>
          {#each group.settings as setting (setting.key)}
            {@const catalog = settingsCatalog[setting.key]}
            <div class="field">
              <div class="field-copy">
                <strong>{catalog.label}</strong>
                <span>{catalog.description}</span>
                {#if setting.restart_required}<small>Restart required</small>{/if}
              </div>

              {#if setting.kind === 'secret'}
                <div class="secret-control">
                  <span>{setting.secret?.configured ? 'Set' : 'Not set'}</span>
                  <label>
                    New {sentenceLabel(catalog.label)}
                    <input
                      type="password"
                      autocomplete="new-password"
                      value={secretValues[setting.key] ?? ''}
                      oninput={(event) => setSecret(setting.key, event.currentTarget.value)}
                    />
                  </label>
                  <button type="button" onclick={() => clearSecret(setting.key)}>
                    Clear {sentenceLabel(catalog.label)}
                  </button>
                </div>
              {:else if optionValues(setting).length > 0}
                <label>
                  <span class="sr-only">{catalog.label}</span>
                  <select
                    value={stringValue(setting)}
                    onchange={(event) => setDraft(setting.key, event.currentTarget.value)}
                  >
                    {#each optionValues(setting) as option}
                      <option value={option}>{option}</option>
                    {/each}
                  </select>
                </label>
              {:else if setting.kind === 'boolean'}
                <label>
                  <span class="sr-only">{catalog.label}</span>
                  <input
                    type="checkbox"
                    checked={Boolean(currentValue(setting))}
                    onchange={(event) => setDraft(setting.key, event.currentTarget.checked)}
                  />
                </label>
              {:else if setting.kind === 'integer' || setting.kind === 'number'}
                <label>
                  <span class="sr-only">{catalog.label}</span>
                  <input
                    type="number"
                    value={stringValue(setting)}
                    step={setting.kind === 'integer' ? '1' : 'any'}
                    oninput={(event) => setDraft(setting.key, Number(event.currentTarget.value))}
                  />
                </label>
              {:else}
                <label>
                  <span class="sr-only">{catalog.label}</span>
                  <input
                    type="text"
                    value={stringValue(setting)}
                    oninput={(event) =>
                      setDraft(
                        setting.key,
                        setting.kind === 'string_array'
                          ? event.currentTarget.value.split(',').map((item) => item.trim()).filter(Boolean)
                          : event.currentTarget.value
                      )}
                  />
                </label>
              {/if}

              {#if onTestConnection && (setting.testable || catalog.testable)}
                <button
                  type="button"
                  aria-label={`Test ${sentenceLabel(catalog.label)} connection`}
                  onclick={() => void onTestConnection(setting.key)}
                >Test connection</button>
              {/if}
            </div>
          {/each}
        </section>
      {/each}

      {#if Object.hasOwn(secretUpdates, 'server.api_key')}
        <label class="confirmation">
          <input type="checkbox" bind:checked={confirmAPIKeyRestart} />
          I understand the API key changes after restart
        </label>
      {/if}

      <button type="submit" disabled={saving}>{saving ? 'Saving…' : 'Save settings'}</button>
    </form>
  {/if}
</main>

<style>
  .settings { max-width: 72rem; margin: 0 auto; }
  section { margin-block: 2rem; }
  .field { display: grid; grid-template-columns: minmax(16rem, 1fr) minmax(14rem, 1fr) auto; gap: 1rem; align-items: center; padding: 1rem 0; border-top: 1px solid var(--border-muted); }
  .field-copy { display: grid; gap: 0.25rem; }
  .field-copy span, small { color: var(--text-muted); }
  input:not([type='checkbox']), select { width: 100%; min-height: 2.25rem; }
  .secret-control { display: grid; gap: 0.5rem; }
  .warning, .pending { padding: 0.75rem 1rem; border-left: 0.25rem solid var(--status-warning-ink); background: var(--status-warning-bg); color: var(--status-warning-ink); }
  .error { padding: 0.75rem 1rem; border-left: 0.25rem solid var(--status-error-ink); background: var(--status-error-bg); color: var(--status-error-ink); }
  .sr-only { position: absolute; width: 1px; height: 1px; padding: 0; margin: -1px; overflow: hidden; clip: rect(0, 0, 0, 0); white-space: nowrap; border: 0; }
  .confirmation { display: block; margin-block: 1rem; }
</style>
