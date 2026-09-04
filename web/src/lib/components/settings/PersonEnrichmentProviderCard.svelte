<script lang="ts">
  import { Button, Toggle } from '@kenn-io/kit-ui';
  import { untrack } from 'svelte';

  import type { APIClient } from '../../api/client';
  import type { components } from '../../api/generated/schema';
  import ProviderCredentialControl from './ProviderCredentialControl.svelte';

  type ProviderSetting = components['schemas']['PersonEnrichmentProviderSetting'];
  type ProviderUpdate = components['schemas']['PersonEnrichmentProviderUpdate'];
  type SettingsDocument = components['schemas']['SettingsResponse'];
  type CredentialResponse = components['schemas']['ProviderCredentialResponse'];

  let {
    client,
    provider,
    configETag,
    credentialETag,
    onSaved,
    onConfigConflict,
    onCredentialSaved,
    onCredentialConflict
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
    draft.endpoint !== provider.endpoint || draft.poll_endpoint !== provider.poll_endpoint
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
      const { data, error: responseError, response } = await client.PUT(
        '/api/v1/settings/person-enrichment/providers/{name}',
        {
          params: {
            path: { name: provider.name },
            header: { 'If-Match': configETag }
          },
          body: providerUpdate(draft)
        }
      );
      if (response.status === 412) {
        await onConfigConflict();
        error = 'The configuration changed on disk. Latest settings were loaded; review this provider and save again.';
        return;
      }
      if (!data) {
        error = apiErrorMessage(responseError, 'Unable to save person-enrichment provider.');
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
      error = cause instanceof Error ? cause.message : 'Unable to save person-enrichment provider.';
    } finally {
      saving = false;
    }
  }

  function setString(field: keyof ProviderSetting, value: string) {
    dirty = true;
    draft = { ...draft, [field]: value };
  }

  function setNumber(field: keyof ProviderSetting, value: string) {
    dirty = true;
    draft = { ...draft, [field]: Number(value) };
  }

  function setList(field: 'allowed_identifiers' | 'target_keys', value: string) {
    dirty = true;
    draft = {
      ...draft,
      [field]: value.split(',').map((item) => item.trim()).filter(Boolean)
    };
  }

  function providerUpdate(value: ProviderSetting): ProviderUpdate {
    return {
      kind: value.kind,
      enabled: value.enabled,
      endpoint: value.endpoint,
      poll_endpoint: value.poll_endpoint,
      mode: value.mode,
      tier: value.tier,
      num_results: value.num_results,
      allowed_identifiers: value.allowed_identifiers ?? [],
      target_keys: value.target_keys ?? [],
      allow_sensitive_targets: value.allow_sensitive_targets,
      retention_posture: value.retention_posture,
      training_posture: value.training_posture,
      refresh_interval: value.refresh_interval,
      request_timeout: value.request_timeout,
      poll_interval: value.poll_interval,
      max_job_age: value.max_job_age,
      max_retries: value.max_retries,
      max_requests_per_run: value.max_requests_per_run,
      max_requests_per_day: value.max_requests_per_day
    };
  }

  function cloneProvider(value: ProviderSetting): ProviderSetting {
    return {
      ...value,
      allowed_identifiers: [...(value.allowed_identifiers ?? [])],
      target_keys: [...(value.target_keys ?? [])]
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
    <div>
      <h4 id={`provider-${provider.name}`}>{provider.name}</h4>
      <p>{provider.kind === 'exa' ? 'Exa' : 'SixtyFour'} · stable provider name</p>
    </div>
    <Toggle
      ariaLabel={`Enable ${provider.name}`}
      checked={draft.enabled}
      onchange={(enabled) => { dirty = true; draft = { ...draft, enabled }; }}
    />
  </header>

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
        <input
          type="number"
          min="1"
          value={draft.num_results ?? 1}
          oninput={(event) => setNumber('num_results', event.currentTarget.value)}
        />
      </label>
    {/if}
    <label class="wide">
      Allowed identifiers
      <input
        value={(draft.allowed_identifiers ?? []).join(', ')}
        oninput={(event) => setList('allowed_identifiers', event.currentTarget.value)}
      />
      <small>Comma-separated identity classes sent to this provider.</small>
    </label>
    <label class="wide">
      Target keys
      <input
        value={(draft.target_keys ?? []).join(', ')}
        oninput={(event) => setList('target_keys', event.currentTarget.value)}
      />
      <small>Only these approved person fields may be written.</small>
    </label>
    <label>
      Retention posture
      <input
        required
        value={draft.retention_posture}
        oninput={(event) => setString('retention_posture', event.currentTarget.value)}
      />
    </label>
    <label>
      Training posture
      <input
        required
        value={draft.training_posture}
        oninput={(event) => setString('training_posture', event.currentTarget.value)}
      />
    </label>
    <label>
      Refresh interval
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
        Poll interval
        <input
          required
          value={draft.poll_interval}
          oninput={(event) => setString('poll_interval', event.currentTarget.value)}
        />
      </label>
      <label>
        Maximum job age
        <input
          required
          value={draft.max_job_age}
          oninput={(event) => setString('max_job_age', event.currentTarget.value)}
        />
      </label>
    {/if}
    <label>
      Maximum retries
      <input
        type="number"
        min="0"
        value={draft.max_retries}
        oninput={(event) => setNumber('max_retries', event.currentTarget.value)}
      />
    </label>
    <label>
      Requests per run
      <input
        type="number"
        min="1"
        value={draft.max_requests_per_run}
        oninput={(event) => setNumber('max_requests_per_run', event.currentTarget.value)}
      />
    </label>
    <label>
      Requests per day
      <input
        type="number"
        min="1"
        value={draft.max_requests_per_day}
        oninput={(event) => setNumber('max_requests_per_day', event.currentTarget.value)}
      />
    </label>
    <div class="sensitive-targets">
      <div>
        <strong>Allow sensitive targets</strong>
        <small>Only enable when every listed target is intentionally approved for provider disclosure.</small>
      </div>
      <Toggle
        ariaLabel={`Allow sensitive targets for ${provider.name}`}
        checked={draft.allow_sensitive_targets}
        onchange={(allow_sensitive_targets) => {
          dirty = true;
          draft = { ...draft, allow_sensitive_targets };
        }}
      />
    </div>
  </div>

  <section class="provider-credential" aria-label={`${provider.name} credential`}>
    <h5>Provider credential</h5>
    <ProviderCredentialControl
      {client}
      credentialID={provider.credential_id}
      label={`${provider.kind === 'exa' ? 'Exa' : 'SixtyFour'} API key for ${provider.name}`}
      credentialState={provider.credential}
      {credentialETag}
      disabledReason={endpointDirty ? 'Save provider settings first before changing this credential.' : ''}
      onSaved={onCredentialSaved}
      onConflict={onCredentialConflict}
    />
  </section>

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
  .provider-card { display: grid; gap: var(--space-4); padding: var(--space-4); border: 1px solid var(--border-muted); border-radius: var(--radius-md); }
  header, footer, .sensitive-targets { display: flex; align-items: center; justify-content: space-between; gap: var(--space-4); }
  h4, h5, p { margin: 0; }
  header p, label small, .sensitive-targets small { color: var(--text-muted); }
  .provider-grid { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: var(--space-3); }
  label { display: grid; gap: 0.35rem; min-width: 0; }
  .wide, .sensitive-targets { grid-column: 1 / -1; }
  input { min-width: 0; min-height: 2.25rem; width: 100%; }
  .provider-credential { display: grid; gap: var(--space-2); padding-top: var(--space-3); border-top: 1px solid var(--border-muted); }
  .provider-error { color: var(--status-error-ink); }

  @media (max-width: 760px) {
    .provider-grid { grid-template-columns: 1fr; }
    .wide, .sensitive-targets { grid-column: auto; }
  }
</style>
