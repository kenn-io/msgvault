<script lang="ts">
  import { putSettingsPersonEnrichmentProvider as generatedPutSettingsPersonEnrichmentProvider } from '../../api/generated/api/api';
  import { Button, TextInput } from '@kenn-io/kit-ui';
  import type { APIClient } from '../../api/client';
  import type {
    PersonEnrichmentProviderUpdate as GeneratedPersonEnrichmentProviderUpdate,
    SettingsResponse as GeneratedSettingsResponse,
  } from '../../api/generated/models';
  type ProviderKind = GeneratedPersonEnrichmentProviderUpdate['kind'];
  type ProviderUpdate = GeneratedPersonEnrichmentProviderUpdate;
  type SettingsDocument = GeneratedSettingsResponse;
  let {
    client,
    kind,
    existingNames,
    configETag,
    onSaved,
    onConflict,
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
      const {
        data,
        error: responseError,
        response,
      } = await generatedPutSettingsPersonEnrichmentProvider({ name: stableName }, canonicalProvider(kind), {
        ...client,
        headers: { 'If-Match': configETag },
      });
      if (response.status === 412) {
        await onConflict();
        error = 'The settings changed on disk. The latest are loaded. Try again.';
        return;
      }
      if (!data) {
        error = apiErrorMessage(responseError, `Can't add ${providerLabel}.`);
        return;
      }
      name = '';
      expanded = false;
      onSaved(data, response.headers.get('ETag') ?? configETag);
    } catch (cause) {
      error = cause instanceof Error ? cause.message : `Can't add ${providerLabel}.`;
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
    if (!stableName) return 'Enter a name.';
    if (!/^[A-Za-z0-9._:-]+$/.test(stableName)) {
      return 'Names can use letters, digits, dots, underscores, colons, and hyphens.';
    }
    if (names.includes(stableName)) return `${stableName} already exists.`;
    return '';
  }
  function canonicalProvider(providerKind: ProviderKind): ProviderUpdate {
    const common = {
      kind: providerKind,
      enabled: false,
      allowed_identifiers: providerKind === 'exa' ? ['public_profile_url'] : ['name', 'current_company'],
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
      max_requests_per_day: 100,
    } satisfies Omit<ProviderUpdate, 'endpoint'>;
    if (providerKind === 'exa') {
      return {
        ...common,
        endpoint: 'https://api.exa.ai/search',
        mode: 'people',
        num_results: 1,
      };
    }
    return {
      ...common,
      endpoint: 'https://api.sixtyfour.ai/people-intelligence-async',
      poll_endpoint: 'https://api.sixtyfour.ai/job-status',
      tier: '',
    };
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
</script>

{#if expanded}
  <form
    class="provider-creator"
    aria-label={`Add ${providerLabel}`}
    onsubmit={(event) => {
      event.preventDefault();
      void createProvider();
    }}
  >
    <label>
      {providerLabel} name
      <TextInput bind:value={name} ariaLabel={`${providerLabel} name`} autocomplete="off" disabled={saving} required block />
    </label>
    {#if error}<small class="creator-error" role="alert">{error}</small>{/if}
    <div class="creator-actions">
      <Button disabled={saving} label="Cancel" onclick={cancel} />
      <Button
        type="submit"
        tone="info"
        surface="solid"
        disabled={saving}
        label={saving ? 'Adding…' : `Add ${providerLabel}`}
      />
    </div>
  </form>
{:else}
  <Button
    size="sm"
    label={`Add ${providerLabel}`}
    onclick={() => {
      expanded = true;
    }}
  />
{/if}

<style>
  .provider-creator {
    display: grid;
    gap: var(--field-gap);
    padding: var(--pane-pad-x);
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-md);
  }
  label {
    display: grid;
    gap: var(--label-gap);
    color: var(--text-secondary);
    font-size: var(--font-size-xs);
  }
  .creator-actions {
    display: flex;
    flex-wrap: wrap;
    justify-content: flex-end;
    gap: var(--control-gap);
  }
  .creator-error {
    color: var(--status-error-ink);
  }
</style>
