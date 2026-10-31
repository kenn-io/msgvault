<script lang="ts">
  import type { APIClient } from '../api/client';
  import { provideVisualReadiness } from './visual-ready.svelte';
  let { client }: { client: APIClient } = $props();
  // svelte-ignore state_referenced_locally
  const readiness = provideVisualReadiness(client);
  export function read(): boolean { return readiness.ready; }
</script>

<span>Ready: {readiness.ready}, absent: {readiness.absent}</span>
