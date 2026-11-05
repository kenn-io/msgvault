<script lang="ts">
  import { historyProvenance, historySource } from './history-format';
  import { formatShortDate } from '../../util/format';
  import { Button } from '@kenn-io/kit-ui';
  import { untrack } from 'svelte';
  import type { AttributeDefinition, PersonAttributeValue } from '../../api/generated/models';
  import type { DirectoryProfileController } from '../../directory/profile-controller.svelte';
  import { displayAttributeValue, joinDefinitions, isAttributeRevealed } from './attribute-value';

  let { controller, universalIDs, compact = false }: { controller: DirectoryProfileController; universalIDs?: string[]; compact?: boolean } = $props();
  const headingID = $props.id();
  let revealed = $state<Record<string, boolean>>({});
  let owner = untrack(() => controller);
  $effect(() => {
    if (controller === owner) return;
    owner = controller;
    revealed = {};
  });
  const visibleFields = $derived(joinDefinitions(controller.definitions, controller.attributes?.attributes ?? []).filter(field => (!universalIDs || universalIDs.includes(field.definition.universal_id)) && (field.current.length > 0 || field.history.length > 0)));
  function isRevealed(definition: AttributeDefinition): boolean { return isAttributeRevealed(definition, revealed); }
  function toggleReveal(definition: AttributeDefinition): void {
    revealed = { ...revealed, [definition.universal_id]: !revealed[definition.universal_id] };
  }
</script>

  {#each visibleFields as field (field.definition.universal_id)}
    {@const shown = isRevealed(field.definition)}
    <section class="history-field" aria-labelledby={`${headingID}-attribute-title-${field.definition.id}`}>
      <header class="section-header">
        <h3 id={`${headingID}-attribute-title-${field.definition.id}`}>{field.definition.label}</h3>
        {#if field.definition.is_sensitive}
          <Button size="sm" label={shown ? 'Hide' : 'Show'} ariaLabel={`${shown ? 'Hide' : 'Reveal'} ${field.definition.label} values`} onclick={() => toggleReveal(field.definition)} />
        {/if}
      </header>
      {#if field.current.length > 0}
        <ul class="history-rows">
          {#each field.current as value (value.id)}
            {@render ValueRow(field.definition, value, shown)}
          {/each}
        </ul>
      {/if}
      {#if field.history.length > 0}
      <details>
      <summary>{field.history.length} earlier {field.history.length === 1 ? 'value' : 'values'}</summary>
      <ul class="history-rows">
        {#each field.history as value (value.id)}
          {@render ValueRow(field.definition, value, shown)}
        {/each}
      </ul>
      </details>
      {/if}
    </section>
  {/each}
{#snippet ValueRow(definition: AttributeDefinition, value: PersonAttributeValue, shown: boolean)}
  <li class:compact>
    <span class={shown ? '' : 'concealed'}>{shown ? displayAttributeValue(definition, value.value) : 'Hidden'}</span>
    <span class="meta">{#if compact}{historySource(value.source)} · <time datetime={value.created_at}>{formatShortDate(value.created_at)}</time>{:else}{historyProvenance(value)}{/if}</span>
    {#if compact}<details><summary>Details</summary><p>{historyProvenance(value)}</p></details>{/if}
  </li>
{/snippet}
<style>
  .history-field { display: grid; gap: var(--space-2); }
  .section-header { display: flex; align-items: center; gap: var(--space-2); flex-wrap: wrap; justify-content: space-between; }
  h3, ul { margin: 0; }
  summary { cursor: pointer; color: var(--text-secondary); font-size: var(--font-size-sm); }
  .history-field summary { padding: var(--space-2) 0; }
  .history-rows { display: grid; padding: 0; list-style: none; }
  .history-rows li { display: flex; align-items: baseline; justify-content: space-between; gap: var(--field-gap); padding: var(--space-2) 0; border-bottom: 1px solid var(--border-muted); color: var(--text-secondary); }
  .history-rows li.compact { display: grid; grid-template-columns: minmax(0, 1fr) auto; row-gap: var(--space-1); }
  .compact > span:first-child { min-width: 0; overflow-wrap: anywhere; }
  .compact > .meta { text-align: right; }
  .compact > details { grid-column: 1 / -1; }
  .compact p { margin: 0; overflow-wrap: anywhere; }
  .compact time { white-space: nowrap; }
  @media (max-width: 640px) {
    .history-rows li.compact { grid-template-columns: minmax(0, 1fr); }
    .compact > .meta { text-align: left; }
  }
  .history-rows .meta, .concealed { color: var(--text-muted); font-size: var(--font-size-sm); }
</style>
