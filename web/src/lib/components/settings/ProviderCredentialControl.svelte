<script lang="ts">
  import type { SecretSettingState } from '../../api/generated/models';
  import { maskSecret } from '../../settings/secrets';
  import SecretField from './SecretField.svelte';

  type SecretUpdate = { action: 'set'; value: string } | { action: 'clear' };
  let { label, credentialState, pending, onchange }: {
    label: string;
    credentialState?: SecretSettingState;
    pending?: SecretUpdate;
    onchange: (update: SecretUpdate | undefined) => void;
  } = $props();
  const configured = $derived(pending ? pending.action === 'set' : credentialState?.configured ?? false);
  const hint = $derived(pending?.action === 'set' ? maskSecret(pending.value) : pending ? '' : credentialState?.hint ?? '');
</script>

<SecretField
  {label}
  {configured}
  {hint}
  source={credentialState?.source}
  disabled={credentialState?.source === 'environment'}
  onreplace={(value) => { onchange({ action: 'set', value }); return true; }}
  onclear={configured && (pending?.action === 'set' || credentialState?.source === 'stored')
    ? () => onchange(credentialState?.source === 'stored' ? { action: 'clear' } : undefined)
    : undefined}
/>
