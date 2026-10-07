<script lang="ts">
  import { Button } from '@kenn-io/kit-ui';
  import type { APIClient } from '../../api/client';
  import type { EvidenceSelector } from '../../kata/evidence';
  import { kataReadiness } from '../../kata/kata-ready.svelte';
  import KataEvidenceDialog from './KataEvidenceDialog.svelte';

  let { client, selector, defaultTitle = 'Follow up', label = 'Create Kata issue' }: {
    client: APIClient; selector: EvidenceSelector; defaultTitle?: string; label?: string;
  } = $props();
  const kata = kataReadiness();
  let open = $state(false);

</script>

{#if kata?.ready}<Button size="sm" surface="soft" {label} onclick={() => open = true} />{/if}
{#if open}
  <KataEvidenceDialog {client} {selector} {defaultTitle} onclose={() => open = false} />
{/if}
