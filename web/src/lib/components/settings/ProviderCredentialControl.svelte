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
    restartRequired = false,
    onSaved,
    onConflict,
  }: {
    client: APIClient;
    credentialID: string;
    label: string;
    credentialState: SecretState | undefined;
    credentialETag: string;
    disabledReason?: string;
    /** The daemon stores the key at once but reads it only after a restart. */
    restartRequired?: boolean;
    onSaved: (response: CredentialResponse, etag: string) => void;
    onConflict: () => void | Promise<void>;
  } = $props();
  let saving = $state(false);
  let error = $state('');
  // Store the key the dialog handed over. Returns true when the dialog may
  // close; a failure keeps it open with the error inside.
  async function saveCredential(value: string): Promise<boolean> {
    if (!value || saving || disabledReason) return false;
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
        return false;
      }
      if (!data) {
        error = apiErrorMessage(responseError, 'Unable to save provider credential.');
        return false;
      }
      onSaved(data, response.headers.get('ETag') ?? credentialETag);
      return true;
    } catch (cause) {
      error = cause instanceof Error ? cause.message : 'Unable to save provider credential.';
      return false;
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
  function sentenceLabel(text: string): string {
    return text.charAt(0).toLowerCase() + text.slice(1);
  }
</script>

<SecretField
  {label}
  configured={credentialState?.configured ?? false}
  hint={credentialState?.hint ?? ''}
  source={credentialState?.source}
  {saving}
  {disabledReason}
  {error}
  applyNote={restartRequired ? 'Saved right away. The daemon uses it after a restart.' : 'Applies right away.'}
  clearLabel={`Clear stored ${sentenceLabel(label)}`}
  onreplace={saveCredential}
  onclear={credentialState?.source === 'stored' ? () => void clearCredential() : undefined}
/>
