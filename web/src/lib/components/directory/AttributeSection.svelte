<script lang="ts">
  import { historyProvenance } from './history-format';
  import { Disclosure, Button } from '@kenn-io/kit-ui';
  import { untrack } from 'svelte';
  import type { AttributeDefinition } from '../../api/generated/models';
  import type { DirectoryProfileController } from '../../directory/profile-controller.svelte';
  import { displayAttributeValue, joinDefinitions, isAttributeRevealed } from './attribute-value';

  let { controller }: { controller: DirectoryProfileController } = $props();
  let revealed = $state<Record<string, boolean>>({});
  let owner = untrack(() => controller);
  $effect(() => {
    if (controller === owner) return;
    owner = controller;
    revealed = {};
  });
  const visibleFields = $derived(joinDefinitions(controller.definitions, controller.attributes?.attributes ?? []).filter(field => field.current.length > 0 || field.history.length > 0));
  function isRevealed(definition: AttributeDefinition): boolean { return isAttributeRevealed(definition, revealed); }
  function toggleReveal(definition: AttributeDefinition): void {
    revealed = { ...revealed, [definition.universal_id]: !revealed[definition.universal_id] };
  }
</script>

  {#each visibleFields as field (field.definition.universal_id)}
    {@const shown = isRevealed(field.definition)}
    <section class="history-field" aria-labelledby={`attribute-title-${field.definition.id}`}>
      <header class="section-header">
        <h3 id={`attribute-title-${field.definition.id}`}>{field.definition.label}</h3>
        {#if field.definition.is_sensitive}
          <Button size="sm" label={shown ? 'Hide' : 'Show'} ariaLabel={`${shown ? 'Hide' : 'Reveal'} ${field.definition.label} values`} onclick={() => toggleReveal(field.definition)} />
        {/if}
      </header>
      {#if field.current.length > 0}
        <ul class="history-rows">
          {#each field.current as value (value.id)}
            <li><span class={shown ? '' : 'concealed'}>{shown ? displayAttributeValue(field.definition, value.value) : 'Hidden'}</span><span class="meta">{historyProvenance(value)}</span></li>
          {/each}
        </ul>
      {/if}
      {#if field.history.length > 0}
      <Disclosure variant="plain">{#snippet summary()}{field.history.length} earlier {field.history.length === 1 ? 'value' : 'values'}{/snippet}
      <ul class="history-rows">
        {#each field.history as value (value.id)}
          <li>
            <span class={shown ? '' : 'concealed'}>{shown ? displayAttributeValue(field.definition, value.value) : 'Hidden'}</span>
            <span class="meta">{historyProvenance(value)}</span>
          </li>
        {/each}
      </ul>
      </Disclosure>
      {/if}
    </section>
  {/each}
<style>
  .history-field { display: grid; gap: var(--space-2); }
  .section-header { display: flex; align-items: center; gap: var(--space-2); flex-wrap: wrap; justify-content: space-between; }
  h3, ul { margin: 0; }
  .history-field :global(.kit-disclosure__summary) { color: var(--text-secondary); }
  .history-rows { display: grid; padding: 0; list-style: none; }
  .history-rows li { display: flex; align-items: baseline; justify-content: space-between; gap: var(--field-gap); padding: var(--space-2) 0; border-bottom: 1px solid var(--border-muted); color: var(--text-secondary); }
  .history-rows .meta, .concealed { color: var(--text-muted); font-size: var(--font-size-sm); }
</style>
