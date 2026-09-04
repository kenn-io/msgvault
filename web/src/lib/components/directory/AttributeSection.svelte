<script lang="ts">
  import { Button } from '@kenn-io/kit-ui';
  import { untrack } from 'svelte';

  import type { components } from '../../api/generated/schema';
  import type { DirectoryProfileController } from '../../directory/profile-controller.svelte';
  import AttributeDefinitionDialog from './AttributeDefinitionDialog.svelte';
  import AttributeEditor from './AttributeEditor.svelte';

  type AttributeDefinition = components['schemas']['AttributeDefinition'];
  type AttributeGroup = components['schemas']['PersonAttributeGroup'];
  type AttributeValue = components['schemas']['AttributeValue'];
  type PersonAttributeValue = components['schemas']['PersonAttributeValue'];

  interface Props {
    controller: DirectoryProfileController;
  }

  interface JoinedAttribute {
    definition: AttributeDefinition;
    current: PersonAttributeValue[];
    history: PersonAttributeValue[];
  }

  let { controller }: Props = $props();
  let editing = $state<{ universalID: string; current?: PersonAttributeValue }>();
  let confirming = $state<{ universalID: string; current: PersonAttributeValue; position: number }>();
  let revealed = $state<Record<string, boolean>>({});
  let creatingDefinition = $state(false);
  let owner = untrack(() => controller);

  $effect(() => {
    if (controller === owner) return;
    owner = controller;
    editing = undefined;
    confirming = undefined;
    revealed = {};
    creatingDefinition = false;
  });

  const fields = $derived.by(() => joinDefinitions(controller.definitions, controller.attributes?.attributes ?? []));

  function joinDefinitions(definitions: AttributeDefinition[], groups: AttributeGroup[]): JoinedAttribute[] {
    const byUniversalID = new Map(groups.map((group) => [group.definition.universal_id, group]));
    const joined = new Map<string, JoinedAttribute>();
    for (const definition of definitions) {
      const group = byUniversalID.get(definition.universal_id);
      joined.set(definition.universal_id, {
        definition,
        current: [...(group?.current ?? [])],
        history: historical(group?.history ?? [])
      });
    }
    for (const group of groups) {
      if (joined.has(group.definition.universal_id)) continue;
      joined.set(group.definition.universal_id, {
        definition: group.definition,
        current: [...(group.current ?? [])],
        history: historical(group.history ?? [])
      });
    }
    return [...joined.values()].sort((left, right) =>
      left.definition.display_order - right.definition.display_order || left.definition.slug.localeCompare(right.definition.slug)
    );
  }

  function historical(values: PersonAttributeValue[]): PersonAttributeValue[] {
    return values.filter((value) => value.active_until !== undefined || value.superseded_at !== undefined);
  }

  function isRevealed(definition: AttributeDefinition): boolean {
    return !definition.is_sensitive || revealed[definition.universal_id] === true;
  }

  function toggleReveal(definition: AttributeDefinition): void {
    const next = !revealed[definition.universal_id];
    revealed = { ...revealed, [definition.universal_id]: next };
    if (!next) discardEditor(definition);
  }

  function discardEditor(definition: AttributeDefinition): void {
    controller.discardAttributeDraft(definition.slug);
    if (editing?.universalID === definition.universal_id) editing = undefined;
    if (confirming?.universalID === definition.universal_id) confirming = undefined;
  }

  function displayValue(definition: AttributeDefinition, value: AttributeValue): string {
    const canonical = rawValue(value);
    const choice = definition.options?.choices?.find((candidate) => candidate.value === canonical);
    return choice?.label ?? canonical;
  }

  function rawValue(value: AttributeValue): string {
    switch (value.type) {
      case 'text': return value.text ?? '—';
      case 'integer': return value.integer?.toString() ?? '—';
      case 'real': return value.real?.toString() ?? '—';
      case 'boolean': return value.boolean === undefined ? '—' : value.boolean ? 'Yes' : 'No';
      case 'date': return value.date ?? '—';
      case 'timestamp': return value.timestamp ?? '—';
      case 'record_reference': return value.record_type === 'person' && value.record_id ? `Person ${value.record_id}` : '—';
      default: return value.json === undefined ? '—' : JSON.stringify(value.json);
    }
  }

  function provenance(value: PersonAttributeValue): string {
    return [
      `Source: ${value.source}`,
      value.actor ? `Actor: ${value.actor}` : undefined,
      value.source_ref ? `Reference: ${value.source_ref}` : undefined,
      value.confidence === undefined ? undefined : `Confidence: ${value.confidence}`,
      `Valid from: ${value.active_from}`,
      value.active_until ? `Valid until: ${value.active_until}` : undefined,
      value.superseded_at ? `Superseded: ${value.superseded_at}` : undefined,
      `Created: ${value.created_at}`
    ].filter(Boolean).join(' · ');
  }

  function metadata(definition: AttributeDefinition): string {
    return [
      `Type: ${definition.value_type}`,
      `Field: ${definition.field_type}`,
      `Cardinality: ${definition.cardinality}`,
      `Ownership: ${definition.ownership}`
    ].join(' · ');
  }

  function operationNeedsReload(definition: AttributeDefinition): boolean {
    const conflict = attributeConflict(definition);
    return conflict?.code === 'attribute_conflict' || conflict?.code === 'precondition_required';
  }

  function attributeConflict(definition: AttributeDefinition) {
    const draft = controller.draft;
    if (draft?.kind !== 'setAttribute' && draft?.kind !== 'clearAttribute') return null;
    return draft.slug === definition.slug ? controller.conflict : null;
  }

  function supported(definition: AttributeDefinition): boolean {
    if (definition.options?.choices?.length) {
      return ['text', 'integer', 'real', 'boolean', 'date', 'timestamp'].includes(definition.value_type);
    }
    return definition.value_type === 'boolean' || definition.value_type === 'integer' || definition.value_type === 'real' ||
      definition.value_type === 'date' || definition.value_type === 'timestamp' || definition.value_type === 'json' || definition.value_type === 'text' ||
      (definition.value_type === 'record_reference' && definition.record_target === 'person');
  }

  function canAdd(field: JoinedAttribute): boolean {
    const definition = field.definition;
    return definition.is_active && definition.api_mutable && definition.ui_creatable && supported(definition) &&
      (!definition.is_sensitive || isRevealed(definition)) &&
      (definition.cardinality === 'multi' || field.current.length === 0) && !controller.mutationPending &&
      !controller.reloadPending && !controller.hasUnresolvedConflict && !operationNeedsReload(definition);
  }

  function canEdit(definition: AttributeDefinition): boolean {
    return definition.is_active && definition.api_mutable && definition.ui_editable && supported(definition) &&
      !controller.mutationPending && !controller.reloadPending && !controller.hasUnresolvedConflict && !operationNeedsReload(definition);
  }

  function canClose(definition: AttributeDefinition): boolean {
    // The store deliberately permits retiring a current value from an
    // inactive definition, but derived/non-API-mutable definitions remain
    // non-retractable.
    return definition.api_mutable && definition.ui_editable && !controller.mutationPending &&
      !controller.reloadPending && !controller.hasUnresolvedConflict && !operationNeedsReload(definition);
  }

  function openEditor(field: JoinedAttribute, current: PersonAttributeValue | undefined = undefined): void {
    if (current ? !canEdit(field.definition) : !canAdd(field)) return;
    editing = { universalID: field.definition.universal_id, ...(current ? { current } : {}) };
    confirming = undefined;
  }

  async function closeCurrent(): Promise<void> {
    if (!confirming) return;
    const field = fields.find((candidate) => candidate.definition.universal_id === confirming?.universalID);
    if (!field || !canClose(field.definition)) return;
    const pending = confirming;
    const result = await controller.clearAttribute(
      field.definition.slug,
      pending.current.id,
      field.definition.cardinality === 'multi' ? pending.current.ordinal : undefined
    );
    if (result === undefined && controller.draft === null && controller.conflict === null) confirming = undefined;
  }

  async function reload(): Promise<void> {
    const result = await controller.reload();
    if (result.ok && controller.draft === null) confirming = undefined;
  }

  function canCreateDefinition(): boolean {
    return !controller.mutationPending && !controller.reloadPending && !controller.hasUnresolvedConflict;
  }
</script>

<section class="attribute-section" aria-label="Attributes">
  <header class="section-header">
    <h3>Attributes</h3>
    <Button label="Create attribute field" size="sm" disabled={!canCreateDefinition()} onclick={() => { creatingDefinition = true; }} />
  </header>

  {#if creatingDefinition}
    <AttributeDefinitionDialog {controller} onClose={() => { creatingDefinition = false; }} />
  {/if}

  {#each fields as field (field.definition.universal_id)}
    <section class="attribute-field" aria-labelledby={`attribute-title-${field.definition.id}`}>
      <header class="field-header">
        <div class="definition-copy">
          <div class="title-row">
            <h4 id={`attribute-title-${field.definition.id}`}>{field.definition.label}</h4>
            {#if field.definition.is_sensitive}<span class="sensitive">Sensitive</span>{/if}
          </div>
          {#if field.definition.description}<p>{field.definition.description}</p>{/if}
          <small>{metadata(field.definition)}</small>
          {#if field.definition.options?.choices?.length}
            <small>Allowed choices: {field.definition.options.choices.map((choice) => choice.label).join(', ')}</small>
          {/if}
          {#if field.definition.derived_source}<small>Computed by {field.definition.derived_source}</small>{/if}
        </div>
        <div class="field-actions">
          {#if field.definition.is_sensitive}
            <Button
              label={`${isRevealed(field.definition) ? 'Hide' : 'Reveal'} ${field.definition.label} values`}
              size="sm"
              onclick={() => toggleReveal(field.definition)}
            />
          {/if}
          <Button
            label={`Add ${field.definition.label} value`}
            size="sm"
            disabled={!canAdd(field)}
            onclick={() => openEditor(field)}
          />
        </div>
      </header>

      <ul class="current-values">
        {#each field.current as value, index (value.id)}
          <li>
            <div class="value-copy">
              {#if isRevealed(field.definition)}
                <strong>{displayValue(field.definition, value.value)}</strong>
              {:else}
                <strong>Sensitive value concealed.</strong>
              {/if}
              <small>{provenance(value)}</small>
            </div>
            <div class="value-actions">
              {#if isRevealed(field.definition)}
                <Button
                  label={`Edit ${field.definition.label} value ${index + 1}`}
                  shortLabel="Edit"
                  size="sm"
                  disabled={!canEdit(field.definition)}
                  onclick={() => openEditor(field, value)}
                />
                <Button
                  label={`Close ${field.definition.label} value ${index + 1}`}
                  shortLabel="Close"
                  size="sm"
                  tone="danger"
                  disabled={!canClose(field.definition)}
                  onclick={() => { confirming = { universalID: field.definition.universal_id, current: value, position: index + 1 }; editing = undefined; }}
                />
              {/if}
            </div>
            {#if confirming?.universalID === field.definition.universal_id && confirming.current.id === value.id}
              <div class="close-confirm" role="group" aria-label={`Confirm closing ${field.definition.label} value ${confirming.position}`}>
                <span>Close this current value while keeping it in history?</span>
                <Button label="Cancel" size="sm" onclick={() => { confirming = undefined; }} />
                <Button label="Confirm close attribute" size="sm" tone="danger" surface="solid" disabled={!canClose(field.definition)} onclick={() => void closeCurrent()} />
              </div>
            {/if}
          </li>
        {:else}
          <li class="empty">No current value.</li>
        {/each}
      </ul>

      {#if editing?.universalID === field.definition.universal_id}
        {#key editing.current?.id ?? 'new'}
          <AttributeEditor
            {controller}
            definition={field.definition}
            current={editing.current}
            sensitiveRevealed={isRevealed(field.definition)}
            onDone={() => { editing = undefined; }}
            onCancel={() => discardEditor(field.definition)}
          />
        {/key}
      {/if}

      {#if field.history.length}
        <details>
          <summary>History ({field.history.length})</summary>
          <ul class="history-values">
            {#each field.history as value (value.id)}
              <li>
                {#if isRevealed(field.definition)}
                  <strong>{displayValue(field.definition, value.value)}</strong>
                {:else}
                  <strong>Sensitive value concealed.</strong>
                {/if}
                <small>{provenance(value)}</small>
              </li>
            {/each}
          </ul>
        </details>
      {/if}

      {#if attributeConflict(field.definition) && editing?.universalID !== field.definition.universal_id}
        <div class="attribute-error" role="alert">
          <span>{controller.conflict?.code === 'attribute_conflict' ? 'This person changed elsewhere. Reload and retry.' : controller.conflict?.message}</span>
          {#if operationNeedsReload(field.definition)}
            <Button label="Reload attributes" size="sm" disabled={!controller.canReload} onclick={() => void reload()} />
          {/if}
        </div>
      {/if}
    </section>
  {:else}
    <p class="empty">No attribute definitions are available.</p>
  {/each}
</section>

<style>
  .attribute-section, .attribute-field, .definition-copy, .value-copy, li { display: grid; gap: var(--space-2); }
  .section-header, .field-header, .field-actions, .title-row, .value-actions, .close-confirm, .attribute-error { display: flex; align-items: center; gap: var(--space-2); flex-wrap: wrap; }
  .section-header { justify-content: space-between; }
  .field-header { align-items: flex-start; justify-content: space-between; }
  .field-actions, .value-actions { justify-content: flex-end; }
  h3, h4, p, ul { margin: 0; }
  h4 { color: var(--text-secondary); font-size: var(--font-size-sm); }
  small, .empty { color: var(--text-muted); font-size: var(--font-size-sm); }
  .attribute-field { padding: var(--space-3); border: 1px solid var(--border-muted); border-radius: var(--radius-sm); }
  .current-values, .history-values { display: grid; gap: var(--space-2); padding: 0; list-style: none; }
  .current-values > li, .history-values > li { padding: var(--space-2); background: var(--bg-inset); border-radius: var(--radius-sm); }
  .sensitive { display: inline-block; padding: 1px 5px; border-radius: var(--radius-sm); background: var(--bg-warning); color: var(--text-primary); font-size: var(--font-size-sm); }
  summary { cursor: pointer; color: var(--text-secondary); font-size: var(--font-size-sm); }
  .history-values { margin-top: var(--space-2); }
  .close-confirm, .attribute-error { padding: var(--space-2); background: var(--bg-inset); color: var(--text-secondary); font-size: var(--font-size-sm); }
  .attribute-error { justify-content: space-between; }
</style>
