<script lang="ts">
  import { Button } from '@kenn-io/kit-ui';

  import type { PersonAttributeGroup } from '../../api/generated/models';
  import { displayAttributeValue } from './attribute-value';

  interface Props {
    groups: PersonAttributeGroup[];
    onEdit?: () => void;
  }

  let { groups, onEdit = undefined }: Props = $props();
  const filled = $derived(groups.filter((group) => (group.current ?? []).length > 0));
</script>

{#if filled.length}
  <section class="attribute-summary" aria-label="Attributes summary">
    <dl>
      {#each filled as group (group.definition.universal_id)}
        <div class="row">
          <dt>{group.definition.label}</dt>
          <dd>
            {#if group.definition.is_sensitive}<span class="sensitive">concealed</span>
            {:else}{(group.current ?? []).map((value) => displayAttributeValue(group.definition, value.value)).join(', ')}{/if}
          </dd>
        </div>
      {/each}
    </dl>
    {#if onEdit}<Button label="Edit attributes" surface="soft" size="sm" onclick={onEdit} />{/if}
  </section>
{/if}

<style>
  .attribute-summary { display: grid; gap: var(--space-2); }
  dl { display: grid; grid-template-columns: max-content 1fr; gap: var(--space-1) var(--space-3); margin: 0; }
  .row { display: contents; }
  dt { color: var(--text-muted); font-size: var(--font-size-sm); }
  dd { margin: 0; overflow-wrap: anywhere; }
  .sensitive { color: var(--text-muted); font-style: italic; }
</style>
