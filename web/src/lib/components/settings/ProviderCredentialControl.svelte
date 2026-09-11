<script lang="ts">
  import {
    deleteSettingsProviderCredential as generatedDeleteSettingsProviderCredential,
    putSettingsProviderCredential as generatedPutSettingsProviderCredential,
  } from '../../api/generated/api/api';
  import SecretField from './SecretField.svelte';
  import type { APIClient } from '../../api/client';
  import type {
    ProviderCredentialResponse as GeneratedProviderCredentialResponse,
    SecretSettingState as GeneratedSecretSettingState,
  } from '../../api/generated/models';
  type SecretState = GeneratedSecretSettingState;
  type CredentialResponse = GeneratedProviderCredentialResponse;
  let {
    client,
    credentialID,
    label,
    credentialState,
    credentialETag,
    disabledReason = '',
    onSaved,
    onConflict,
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
      const {
        data,
        error: responseError,
        response,
      } = await generatedPutSettingsProviderCredential(
        { credentialId: credentialID },
        { value },
        {
          ...client,
          headers: { 'If-Match': credentialETag },
        },
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
      const {
        data,
        error: responseError,
        response,
      } = await generatedDeleteSettingsProviderCredential(
        { credentialId: credentialID },
        {
          ...client,
          headers: { 'If-Match': credentialETag },
        },
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
      const message = (
        responseError as {
          message?: unknown;
        }
      ).message;
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

<SecretField
  {label}
  status={sourceLabel(credentialState)}
  unset={!credentialState?.configured}
  bind:value
  {saving}
  {disabledReason}
  {error}
  clearLabel={`Clear stored ${sentenceLabel(label)}`}
  onsave={() => void saveCredential()}
  onclear={credentialState?.source === 'stored' ? () => void clearCredential() : undefined}
/>
