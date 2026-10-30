<script lang="ts">
  import { Chip } from '@kenn-io/kit-ui';

  import { RUN_STATES, type RunState } from './run-state';

  let { state }: { state: RunState } = $props();

  const entry = $derived(RUN_STATES[state]);
</script>

<Chip size="sm" tone={entry.tone} dot uppercase={false}>{entry.label}</Chip>
