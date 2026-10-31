<script lang="ts">
  import { putSettingsPersonEnrichmentProvider as generatedPutSettingsPersonEnrichmentProvider } from '../../api/generated/api/api';
  import { Button, Chip, NumberInput, Toggle } from '@kenn-io/kit-ui';
  import { untrack } from 'svelte';
  import type { APIClient } from '../../api/client';
  import type {
    PersonEnrichmentProviderSetting as ProviderSetting,
    ProviderCredentialResponse as CredentialResponse,
    SettingsResponse as SettingsDocument,
  } from '../../api/generated/models';
  import { providerUpdate } from '../../settings/catalog';
  import ProviderCredentialControl from './ProviderCredentialControl.svelte';
  let {
    client,
    provider,
    configETag,
    credentialETag,
    onSaved,
    onConfigConflict,
    onCredentialSaved,
    onCredentialConflict,
  }: {
    client: APIClient;
    provider: ProviderSetting;
    configETag: string;
    credentialETag: string;
    onSaved: (document: SettingsDocument, etag: string) => void;
    onConfigConflict: () => void | Promise<void>;
    onCredentialSaved: (response: CredentialResponse, etag: string) => void;
    onCredentialConflict: () => void | Promise<void>;
  } = $props();
  let draft = $state<ProviderSetting>(cloneProvider(untrack(() => provider)));
  let observedProvider = $state<ProviderSetting>(untrack(() => provider));
  let dirty = $state(false);
  let saving = $state(false);
  let error = $state('');
  const endpointDirty = $derived(
    draft.endpoint !== provider.endpoint || draft.poll_endpoint !== provider.poll_endpoint,
  );
  $effect(() => {
    const next = provider;
    if (next === observedProvider) return;
    observedProvider = next;
    if (!dirty) draft = cloneProvider(next);
  });
  async function saveProvider() {
    if (saving) return;
    saving = true;
    error = '';
    try {
      const {
        data,
        error: responseError,
        response,
      } = await generatedPutSettingsPersonEnrichmentProvider({ name: provider.name }, providerUpdate(draft), {
        ...client,
        headers: { 'If-Match': configETag },
      });
      if (response.status === 412) {
        await onConfigConflict();
        error = 'The settings changed on disk. The latest are loaded. Review this provider and save again.';
        return;
      }
      if (!data) {
        error = apiErrorMessage(responseError, "Can't save this provider.");
        return;
      }
      const saved = data.person_enrichment_providers?.find((candidate) => candidate.name === provider.name);
      if (saved) {
        draft = cloneProvider(saved);
        observedProvider = saved;
      }
      dirty = false;
      onSaved(data, response.headers.get('ETag') ?? configETag);
    } catch (cause) {
      error = cause instanceof Error ? cause.message : "Can't save this provider.";
    } finally {
      saving = false;
    }
  }
  function edit(next: ProviderSetting) {
    dirty = true;
    draft = next;
  }
  function setString(field: keyof ProviderSetting, value: string) {
    edit({ ...draft, [field]: value });
  }
  function setNumber(field: keyof ProviderSetting, value: number | null) {
    edit({ ...draft, [field]: Number(value) });
  }
  function setList(field: 'allowed_identifiers' | 'target_keys', value: string) {
    edit({
      ...draft,
      [field]: value
        .split(',')
        .map((item) => item.trim())
        .filter(Boolean),
    });
  }
  function cloneProvider(value: ProviderSetting): ProviderSetting {
    return {
      ...value,
      allowed_identifiers: [...(value.allowed_identifiers ?? [])],
      target_keys: [...(value.target_keys ?? [])],
    };
  }
  function apiErrorMessage(responseError: unknown, fallback: string): string {
    if (typeof responseError === 'object' && responseError !== null && 'message' in responseError) {
      const message = (responseError as { message?: unknown }).message;
      if (typeof message === 'string' && message) return message;
    }
    return fallback;
  }
</script>
<article class="provider-card" aria-labelledby={`provider-${provider.name}`}>
  <header>
    <h4 id={`provider-${provider.name}`}>{provider.name}</h4>
    <Chip size="xs" tone="muted" uppercase={false}>{provider.kind === 'exa' ? 'Exa' : 'SixtyFour'}</Chip>
    <span class="grow"></span>
    <Toggle
      ariaLabel={`Enable ${provider.name}`}
      checked={draft.enabled}
      onchange={(enabled) => edit({ ...draft, enabled })}
    />
  </header>
  <div class="field" aria-label={`${provider.name} credential`}>
    <span>API key</span>
    <ProviderCredentialControl
      {client}
      credentialID={provider.credential_id}
      label={`API key for ${provider.name}`}
      credentialState={provider.credential}
      {credentialETag}
      disabledReason={endpointDirty ? 'Save provider settings first before changing this credential.' : ''}
      onSaved={onCredentialSaved}
      onConflict={onCredentialConflict}
    />
  </div>
  <details class="advanced">
    <summary>Advanced</summary>
    <div class="provider-grid">
      <label class="wide">
        Endpoint
        <input
          type="url"
          required
          aria-label={`${provider.name} endpoint`}
          value={draft.endpoint}
          oninput={(event) => setString('endpoint', event.currentTarget.value)}
        />
      </label>
      {#if provider.kind === 'sixtyfour'}
        <label class="wide">
          Poll endpoint
          <input
            type="url"
            required
            aria-label={`${provider.name} poll endpoint`}
            value={draft.poll_endpoint ?? ''}
            oninput={(event) => setString('poll_endpoint', event.currentTarget.value)}
          />
        </label>
        <label>
          Tier
          <input value={draft.tier ?? ''} oninput={(event) => setString('tier', event.currentTarget.value)} />
        </label>
      {:else}
        <label>
          Search mode
          <input value={draft.mode ?? ''} oninput={(event) => setString('mode', event.currentTarget.value)} />
        </label>
        <label>
          Results per request
          <NumberInput
            block
            min={1}
            value={draft.num_results ?? 1}
            oninput={(value) => setNumber('num_results', value)}
          />
        </label>
      {/if}
      <label class="wide">
        Details sent to the provider
        <input
          value={(draft.allowed_identifiers ?? []).join(', ')}
          oninput={(event) => setList('allowed_identifiers', event.currentTarget.value)}
        />
      </label>
      <label class="wide">
        Fields it may fill in
        <input
          value={(draft.target_keys ?? []).join(', ')}
          oninput={(event) => setList('target_keys', event.currentTarget.value)}
        />
      </label>
      <label>
        How long the provider keeps data
        <input
          required
          value={draft.retention_posture}
          oninput={(event) => setString('retention_posture', event.currentTarget.value)}
        />
      </label>
      <label>
        Whether the provider trains on data
        <input
          required
          value={draft.training_posture}
          oninput={(event) => setString('training_posture', event.currentTarget.value)}
        />
      </label>
      <label>
        Refresh every
        <input
          required
          value={draft.refresh_interval}
          oninput={(event) => setString('refresh_interval', event.currentTarget.value)}
        />
      </label>
      <label>
        Request timeout
        <input
          required
          value={draft.request_timeout}
          oninput={(event) => setString('request_timeout', event.currentTarget.value)}
        />
      </label>
      {#if provider.kind === 'sixtyfour'}
        <label>
          Check for results every
          <input
            required
            value={draft.poll_interval}
            oninput={(event) => setString('poll_interval', event.currentTarget.value)}
          />
        </label>
        <label>
          Give up after
          <input
            required
            value={draft.max_job_age}
            oninput={(event) => setString('max_job_age', event.currentTarget.value)}
          />
        </label>
      {/if}
      <label>
        Retries
        <NumberInput
          block
          min={0}
          value={draft.max_retries}
          oninput={(value) => setNumber('max_retries', value)}
        />
      </label>
      <label>
        Requests per run
        <NumberInput
          block
          min={1}
          value={draft.max_requests_per_run}
          oninput={(value) => setNumber('max_requests_per_run', value)}
        />
      </label>
      <label>
        Requests per day
        <NumberInput
          block
          min={1}
          value={draft.max_requests_per_day}
          oninput={(value) => setNumber('max_requests_per_day', value)}
        />
      </label>
      <div class="sensitive-targets">
        <span>Allow sensitive fields</span>
        <Toggle
          ariaLabel={`Allow sensitive fields for ${provider.name}`}
          checked={draft.allow_sensitive_targets}
          onchange={(allow_sensitive_targets) => edit({ ...draft, allow_sensitive_targets })}
        />
      </div>
    </div>
  </details>
  {#if error}<p class="provider-error" role="alert">{error}</p>{/if}
  <footer>
    <Button
      disabled={saving}
      label={saving ? 'Saving…' : `Save ${provider.name} provider`}
      onclick={() => void saveProvider()}
    />
  </footer>
</article>

<style>
  .provider-card {
    display: grid;
    gap: var(--field-gap);
    padding: var(--pane-pad-x);
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-md);
  }
  header,
  .sensitive-targets {
    display: flex;
    align-items: center;
    gap: var(--control-gap);
  }
  .grow {
    flex: 1;
  }
  h4,
  .provider-error {
    margin: 0;
  }
  h4 {
    font-size: var(--font-size-md);
    font-weight: var(--font-weight-semibold);
  }
  .field {
    display: grid;
    gap: var(--space-2);
    color: var(--text-secondary);
    font-size: var(--font-size-xs);
  }
  footer {
    display: flex;
    justify-content: flex-end;
  }
  .provider-error {
    color: var(--status-error-ink);
    font-size: var(--font-size-sm);
  }
  .advanced summary {
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
    cursor: pointer;
  }
  .provider-grid {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: var(--field-gap) var(--section-gap);
    margin-top: var(--field-gap);
  }
  label {
    display: grid;
    gap: var(--space-2);
    min-width: 0;
    color: var(--text-secondary);
    font-size: var(--font-size-xs);
  }
  .wide,
  .sensitive-targets {
    grid-column: 1 / -1;
  }
  .sensitive-targets {
    justify-content: space-between;
    font-size: var(--font-size-sm);
  }
  input {
    box-sizing: border-box;
    min-width: 0;
    height: var(--control-height);
    width: 100%;
    padding: 0 var(--space-3);
    border: var(--border-width) solid var(--border-default);
    border-radius: var(--radius-md);
    background: var(--bg-surface);
    color: var(--text-primary);
    font: inherit;
    font-size: var(--font-size-sm);
  }

  @media (max-width: 760px) {
    .provider-grid {
      grid-template-columns: 1fr;
    }
    .wide,
    .sensitive-targets {
      grid-column: auto;
    }
  }
</style>
