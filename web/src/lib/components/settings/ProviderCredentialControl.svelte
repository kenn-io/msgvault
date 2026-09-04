<script lang="ts">
  import { Button, TextInput } from '@kenn-io/kit-ui';

  import type { APIClient } from '../../api/client';
  import type { components } from '../../api/generated/schema';

  type SecretState = components['schemas']['SecretSettingState'];
  type CredentialResponse = components['schemas']['ProviderCredentialResponse'];

  let {
    client,
    credentialID,
    label,
    credentialState,
    credentialETag,
    disabledReason = '',
    onSaved,
    onConflict
  }: {
    client: APIClient;
    credentialID: string;
    label: string;
    credentialState: SecretState | undefined;
    credentialETag: string;
    disabledReason?: string;
    onSaved: (response: CredentialResponse, etag: string) => void;
    onConflict: () => void | Promise<void>;
  } = $props();

  let value = $state('');
  let saving = $state(false);
  let error = $state('');

  $effect(() => {
    if (disabledReason) value = '';
  });

  async function saveCredential() {
    if (!value || saving || disabledReason) return;
    saving = true;
    error = '';
    try {
      const { data, error: responseError, response } = await client.PUT(
        '/api/v1/settings/provider-credentials/{credential_id}',
        {
          params: {
            path: { credential_id: credentialID },
            header: { 'If-Match': credentialETag }
          },
          body: { value }
        }
      );
      if (response.status === 412) {
        await onConflict();
        error = 'Provider credentials changed. Reloaded the latest state; enter the credential again.';
        value = '';
        return;
      }
      if (!data) {
        error = apiErrorMessage(responseError, 'Unable to save provider credential.');
        return;
      }
      value = '';
      onSaved(data, response.headers.get('ETag') ?? credentialETag);
    } catch (cause) {
      error = cause instanceof Error ? cause.message : 'Unable to save provider credential.';
    } finally {
      saving = false;
    }
  }

  async function clearCredential() {
    if (saving || disabledReason || credentialState?.source !== 'stored') return;
    saving = true;
    error = '';
    try {
      const { data, error: responseError, response } = await client.DELETE(
        '/api/v1/settings/provider-credentials/{credential_id}',
        {
          params: {
            path: { credential_id: credentialID },
            header: { 'If-Match': credentialETag }
          }
        }
      );
      if (response.status === 412) {
        await onConflict();
        error = 'Provider credentials changed. Reloaded the latest state.';
        return;
      }
      if (!data) {
        error = apiErrorMessage(responseError, 'Unable to clear provider credential.');
        return;
      }
      value = '';
      onSaved(data, response.headers.get('ETag') ?? credentialETag);
    } catch (cause) {
      error = cause instanceof Error ? cause.message : 'Unable to clear provider credential.';
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

  function sourceLabel(secret: SecretState | undefined): string {
    if (secret?.source === 'stored') return 'Stored credential';
    if (secret?.source === 'environment') return 'Environment variable';
    return secret?.configured ? 'Configured' : 'Not configured';
  }

  function sentenceLabel(text: string): string {
    return text.charAt(0).toLowerCase() + text.slice(1);
  }
</script>

<div class="credential-control">
  <span class="credential-source">{sourceLabel(credentialState)}</span>
  <label>
    New {sentenceLabel(label)}
    <TextInput
      type="password"
      autocomplete="new-password"
      bind:value
      disabled={saving || Boolean(disabledReason)}
      block
    />
  </label>
  {#if disabledReason}<small class="credential-blocked">{disabledReason}</small>{/if}
  <div class="credential-actions">
    <Button
      disabled={saving || value === '' || Boolean(disabledReason)}
      label={saving ? 'Saving…' : `Save ${sentenceLabel(label)}`}
      onclick={() => void saveCredential()}
    />
    {#if credentialState?.source === 'stored'}
      <Button
        disabled={saving || Boolean(disabledReason)}
        label={`Clear stored ${sentenceLabel(label)}`}
        onclick={() => void clearCredential()}
      />
    {/if}
  </div>
  {#if error}<small class="credential-error" role="alert">{error}</small>{/if}
</div>

<style>
  .credential-control { display: grid; gap: 0.5rem; width: 100%; min-width: 0; }
  .credential-source { color: var(--text-muted); }
  .credential-blocked { color: var(--status-warning-ink); }
  .credential-actions { display: flex; flex-wrap: wrap; gap: var(--space-2); }
  .credential-error { color: var(--status-error-ink); }
</style>
