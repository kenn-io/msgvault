<script lang="ts">
  import type { SecretSettingState } from '../../api/generated/models';
  import { pendingSecret, type SecretUpdate } from '../../settings/secrets';
  import SecretField from './SecretField.svelte';

  let { label, credentialState, pending, onchange }: {
    label: string;
    credentialState?: SecretSettingState;
    pending?: SecretUpdate;
    onchange: (update: SecretUpdate | undefined) => void;
  } = $props();
  const shown = $derived(pendingSecret(credentialState, pending));
</script>

<SecretField
  {label}
  configured={shown.configured}
  hint={shown.hint}
  source={credentialState?.source}
  onreplace={(value) => { onchange({ action: 'set', value }); return true; }}
  onclear={shown.configured && (pending?.action === 'set' || credentialState?.source === 'stored')
    ? () => onchange(credentialState?.source === 'stored' ? { action: 'clear' } : undefined)
    : undefined}
/>
