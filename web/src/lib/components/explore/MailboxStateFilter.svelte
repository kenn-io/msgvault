<script lang="ts">
  import { Checkbox } from '@kenn-io/kit-ui';
  import type { ExploreFilter } from '../../explore/models';
  import { MAILBOX_STATE_FILTERS } from '../../explore/mailbox-state';

  let { filters, onChange }: { filters: ExploreFilter[]; onChange: (filters: ExploreFilter[]) => void } = $props();

  function toggle(dimension: 'inbox_state' | 'read_state', value: string, checked: boolean): void {
    const current = filters.find(filter => filter.dimension === dimension)?.values ?? [];
    const values = checked ? [...new Set([...current, value])] : current.filter(item => item !== value);
    const rest = filters.filter(filter => filter.dimension !== dimension);
    onChange(values.length ? [...rest, { dimension, values }] : rest);
  }
</script>

<div class="mailbox-filters">
  {#each MAILBOX_STATE_FILTERS as filter (filter.dimension)}
    <fieldset>
      <legend>{filter.label}</legend>
      <div class="options">
        {#each filter.options as option (option.value)}
          <Checkbox label={option.label}
            checked={filters.some(item => item.dimension === filter.dimension && item.values.includes(option.value))}
            onchange={(checked) => toggle(filter.dimension, option.value, checked)} />
        {/each}
      </div>
    </fieldset>
  {/each}
  <p>Last observed by sync. Unknown means provider metadata is missing. Reading in msgvault does not change provider read status.</p>
</div>

<style>
  .mailbox-filters { display: grid; gap: var(--space-3); min-width: 0; }
  fieldset { border: 0; padding: 0; margin: 0; min-width: 0; }
  legend { font-size: var(--font-size-xs); color: var(--text-muted); margin-bottom: var(--space-2); }
  .options { display: flex; flex-wrap: wrap; gap: var(--space-2); }
  p { margin: 0; color: var(--text-muted); font-size: var(--font-size-xs); }
</style>
