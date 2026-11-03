<script lang="ts">
  import type { APIClient } from '../api/client';
  import { provideVisualReadiness } from './visual-ready.svelte';
  let { client }: { client: APIClient } = $props();
  // svelte-ignore state_referenced_locally
  const readiness = provideVisualReadiness(client);
</script>

<span>Ready: {readiness.ready}</span>
