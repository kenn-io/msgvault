<script lang="ts">
  import { Button, TextInput } from '@kenn-io/kit-ui';

  import type { APIClient } from '../../api/client';
  import type { components } from '../../api/generated/schema';

  type ProviderKind = components['schemas']['PersonEnrichmentProviderUpdate']['kind'];
  type ProviderUpdate = components['schemas']['PersonEnrichmentProviderUpdate'];
  type SettingsDocument = components['schemas']['SettingsResponse'];

  let {
    client,
    kind,
    existingNames,
    configETag,
    onSaved,
    onConflict
  }: {
    client: APIClient;
    kind: ProviderKind;
    existingNames: string[];
    configETag: string;
    onSaved: (document: SettingsDocument, etag: string) => void;
    onConflict: () => void | Promise<void>;
  } = $props();

  let expanded = $state(false);
  let name = $state('');
  let saving = $state(false);
  let error = $state('');
  const providerLabel = $derived(kind === 'exa' ? 'Exa' : 'SixtyFour');

  async function createProvider() {
    if (saving) return;
    const stableName = name.trim();
    const invalid = providerNameError(stableName, existingNames);
    if (invalid) {
      error = invalid;
      return;
    }
    saving = true;
    error = '';
    try {
      const { data, error: responseError, response } = await client.PUT(
        '/api/v1/settings/person-enrichment/providers/{name}',
        {
          params: {
            path: { name: stableName },
            header: { 'If-Match': configETag }
          },
          body: canonicalProvider(kind)
        }
      );
      if (response.status === 412) {
        await onConflict();
        error = 'The configuration changed on disk. Latest settings were loaded; review the stable name and try again.';
        return;
      }
      if (!data) {
        error = apiErrorMessage(responseError, `Unable to create ${providerLabel} provider.`);
        return;
      }
      name = '';
      expanded = false;
      onSaved(data, response.headers.get('ETag') ?? configETag);
    } catch (cause) {
      error = cause instanceof Error ? cause.message : `Unable to create ${providerLabel} provider.`;
    } finally {
      saving = false;
    }
  }

  function cancel() {
    name = '';
    error = '';
    expanded = false;
  }

  function providerNameError(stableName: string, names: string[]): string {
    if (!stableName) return 'A stable provider name is required.';
    if (!/^[A-Za-z0-9._:-]+$/.test(stableName)) {
      return "Stable names may use only letters, digits, '.', '_', ':', or '-'.";
    }
    if (names.includes(stableName)) return `A provider named ${stableName} already exists.`;
    return '';
  }

  function canonicalProvider(providerKind: ProviderKind): ProviderUpdate {
    const common = {
      kind: providerKind,
      enabled: false,
      allowed_identifiers: providerKind === 'exa'
        ? ['public_profile_url']
        : ['name', 'current_company'],
      target_keys: ['attribute:location'],
      allow_sensitive_targets: false,
      retention_posture: '',
      training_posture: '',
      refresh_interval: '720h',
      request_timeout: '1m',
      poll_interval: '30s',
      max_job_age: '15m',
      max_retries: 5,
      max_requests_per_run: 10,
      max_requests_per_day: 100
    } satisfies Omit<ProviderUpdate, 'endpoint'>;
    if (providerKind === 'exa') {
      return {
        ...common,
        endpoint: 'https://api.exa.ai/search',
        mode: 'people',
        num_results: 1
      };
    }
    return {
      ...common,
      endpoint: 'https://api.sixtyfour.ai/people-intelligence-async',
      poll_endpoint: 'https://api.sixtyfour.ai/job-status',
      tier: ''
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

<section class="provider-creator" aria-label={`Add ${providerLabel} provider`}>
  {#if expanded}
    <div class="creator-copy">
      <strong>Add {providerLabel} provider</strong>
      <small>The provider starts disabled. Review its disclosure policy and add a credential before enabling it.</small>
    </div>
    <label>
      {providerLabel} stable provider name
      <TextInput
        bind:value={name}
        ariaLabel={`${providerLabel} stable provider name`}
        autocomplete="off"
        disabled={saving}
        required
        block
      />
      <small>Names are durable IDs and cannot change provider kind later.</small>
    </label>
    {#if error}<small class="creator-error" role="alert">{error}</small>{/if}
    <div class="creator-actions">
      <Button disabled={saving} label="Cancel" onclick={cancel} />
      <Button
        disabled={saving}
        label={saving ? 'Creating…' : `Create ${providerLabel} provider`}
        onclick={() => void createProvider()}
      />
    </div>
  {:else}
    <Button label={`Add ${providerLabel} provider`} onclick={() => { expanded = true; }} />
  {/if}
</section>

<style>
  .provider-creator { display: grid; gap: var(--space-3); padding: var(--space-4); border: 1px dashed var(--border-muted); border-radius: var(--radius-md); }
  .creator-copy, label { display: grid; gap: 0.35rem; }
  .creator-copy small, label small { color: var(--text-muted); }
  .creator-actions { display: flex; flex-wrap: wrap; justify-content: flex-end; gap: var(--space-2); }
  .creator-error { color: var(--status-error-ink); }
</style>
