<script lang="ts">
  import { appShortcuts, Button, Checkbox, EmptyState, Modal } from '@kenn-io/kit-ui';
  import { onDestroy, onMount } from 'svelte';

  import type { APIClient } from '../../api/client';
  import type { DirectoryEntityController } from '../../directory/entity-controller.svelte';
  import type { PersonRelationship, PersonRelationshipView, RelationshipType } from '../../directory/models';
  import PersonRelationshipEditor from './PersonRelationshipEditor.svelte';
  import RelationshipTypeEditor from './RelationshipTypeEditor.svelte';

  interface Props {
    client: APIClient;
    controller: DirectoryEntityController;
    personID: number;
  }

  let { client, controller, personID }: Props = $props();
  let relationshipEditor = $state<PersonRelationshipView | null | undefined>(undefined);
  let relationshipTypeEditor = $state<RelationshipType | null | undefined>(undefined);
  let deletingRelationship = $state<PersonRelationshipView>();
  let deletingType = $state<RelationshipType>();
  let acting = $state(false);
  let actionMessage = $state('');
  let conflictRelationship = $state<PersonRelationship>();
  let conflictType = $state<RelationshipType>();
  let releaseScope: (() => void) | undefined;

  onMount(() => { releaseScope = appShortcuts.pushScope('directory-relationships'); });
  onDestroy(() => releaseScope?.());

  function counterpart(view: PersonRelationshipView): string {
    return view.counterpart_display_name?.trim() || view.counterpart_vcard_uid || `Person ${view.counterpart_person_id}`;
  }

  function dateText(value: { year?: number; month?: number; day?: number } | undefined): string {
    if (!value) return '';
    return [value.year?.toString().padStart(4, '0'), value.month?.toString().padStart(2, '0'), value.day?.toString().padStart(2, '0')].filter(Boolean).join('-');
  }

  async function changeHistoryMode(checked: boolean): Promise<void> {
    await controller.refreshRelationships(checked);
  }

  async function removeRelationship(): Promise<void> {
    if (!deletingRelationship || acting) return;
    acting = true;
    actionMessage = '';
    conflictRelationship = undefined;
    try {
      const result = await controller.deleteRelationship(deletingRelationship.relationship.id);
      if (result.ok) {
        deletingRelationship = undefined;
        return;
      }
      if (result.kind === 'conflict') {
        conflictRelationship = result.current;
        actionMessage = `This relationship changed elsewhere. ${result.message}`;
      } else {
        actionMessage = result.message;
      }
    } finally {
      acting = false;
    }
  }

  async function removeRelationshipType(): Promise<void> {
    if (!deletingType || acting) return;
    acting = true;
    actionMessage = '';
    conflictType = undefined;
    try {
      const result = await controller.deleteRelationshipType(deletingType.id);
      if (result.ok) {
        deletingType = undefined;
        return;
      }
      if (result.kind === 'conflict') {
        conflictType = result.current;
        actionMessage = `This relationship type changed elsewhere. ${result.message}`;
      } else {
        actionMessage = result.message;
      }
    } finally {
      acting = false;
    }
  }

  function closeDelete(): void {
    if (acting) return;
    deletingRelationship = undefined;
    deletingType = undefined;
    actionMessage = '';
    conflictRelationship = undefined;
    conflictType = undefined;
  }
</script>

<section class="relationships-tab" aria-label="Person relationships">
  <header>
    <div><h2>Relationships</h2><p>Curated, typed connections between durable people.</p></div>
    <div class="actions">
      <Button label="Add relationship" onclick={() => { relationshipEditor = null; }} />
      <Button label="Add relationship type" onclick={() => { relationshipTypeEditor = null; }} />
    </div>
  </header>

  <div class="filters">
    <Checkbox checked={controller.relationshipsIncludeEnded} label="Include ended relationships" onchange={(checked) => void changeHistoryMode(checked)} />
    <Button label="Refresh relationships" disabled={controller.relationshipsLoading} onclick={() => void controller.refreshRelationships()} />
  </div>

  {#if controller.errors.relationships}
    <div class="refresh-error" role="alert">
      <p>{controller.errors.relationships}</p>
      <Button label="Refresh relationships" disabled={controller.relationshipsLoading} onclick={() => void controller.refreshRelationships()} />
    </div>
  {/if}
  {#if controller.relationshipsLoading}<p role="status">Loading relationships…</p>{/if}
  {#if controller.relationships.length === 0 && !controller.relationshipsLoading && !controller.errors.relationships}
    <EmptyState title="No relationships" description="Add a typed relationship to connect this durable person." />
  {:else if controller.relationships.length > 0}
    <ul class="records">
      {#each controller.relationships as view (view.relationship.id)}
        <li>
          <div>
            <strong>{counterpart(view)} · {view.counterpart_label}</strong>
            <small>{view.direction}{#if view.relationship.start_date} · from {dateText(view.relationship.start_date)}{/if}{#if view.relationship.end_date} · ended {dateText(view.relationship.end_date)}{/if}</small>
            {#if view.relationship.notes}<p>{view.relationship.notes}</p>{/if}
          </div>
          <div class="row-actions">
            <Button size="sm" label="Edit relationship" onclick={() => { relationshipEditor = view; }} />
            <Button size="sm" tone="danger" label="Delete relationship" onclick={() => { deletingRelationship = view; }} />
          </div>
        </li>
      {/each}
    </ul>
  {/if}

  <section aria-label="Relationship types">
    <div class="section-heading"><h3>Relationship types</h3><Button label="Refresh relationship types" disabled={controller.relationshipTypesLoading} onclick={() => void controller.refreshRelationshipTypes()} /></div>
    {#if controller.relationshipTypesLoading}<p role="status">Loading relationship types…</p>
    {:else if controller.errors.relationshipTypes}<p role="alert">{controller.errors.relationshipTypes}</p>
    {:else if controller.relationshipTypes.length === 0}<p class="muted">No relationship types are available.</p>
    {:else}
      <ul class="types">
        {#each controller.relationshipTypes as type (type.id)}
          <li>
            <span><strong>{type.slug}</strong> · {type.is_symmetric ? type.forward_label : `${type.forward_label} / ${type.reverse_label}`} <small>{type.ownership === 'system' ? 'Built in' : 'Custom'}</small></span>
            {#if type.ownership !== 'system'}
              <span class="row-actions">
                <Button size="sm" label={`Edit ${type.slug} relationship type`} onclick={() => { relationshipTypeEditor = type; }} />
                {#if type.is_deletable}<Button size="sm" tone="danger" label={`Delete ${type.slug} relationship type`} onclick={() => { deletingType = type; }} />{/if}
              </span>
            {/if}
          </li>
        {/each}
      </ul>
    {/if}
  </section>
</section>

{#if relationshipEditor !== undefined}
  <PersonRelationshipEditor {client} {controller} {personID} relationship={relationshipEditor ?? undefined}
    onDone={() => { relationshipEditor = undefined; }} onClose={() => { relationshipEditor = undefined; }} />
{/if}
{#if relationshipTypeEditor !== undefined}
  <RelationshipTypeEditor {controller} relationshipType={relationshipTypeEditor ?? undefined}
    onDone={() => { relationshipTypeEditor = undefined; }} onClose={() => { relationshipTypeEditor = undefined; }} />
{/if}
{#if deletingRelationship}
  <Modal title="Delete relationship" ariaLabel="Delete relationship" closeLabel="Close delete relationship" onclose={closeDelete}>
    <p>Delete the relationship with {counterpart(deletingRelationship)}?</p>
    {#if actionMessage}<div role="alert"><p>{actionMessage}</p>{#if conflictRelationship}<p>Current record: {dateText(conflictRelationship.end_date) || 'no end date'} · {conflictRelationship.notes || 'no notes'}.</p>{/if}</div>{/if}
    {#snippet footer()}<Button label="Cancel" disabled={acting} onclick={closeDelete} /><Button tone="danger" surface="solid" label="Confirm delete relationship" disabled={acting} onclick={() => void removeRelationship()} />{/snippet}
  </Modal>
{/if}
{#if deletingType}
  <Modal title="Delete relationship type" ariaLabel="Delete relationship type" closeLabel="Close delete relationship type" onclose={closeDelete}>
    <p>Delete the custom relationship type {deletingType.slug}?</p>
    {#if actionMessage}<div role="alert"><p>{actionMessage}</p>{#if conflictType}<p>Current labels: {conflictType.forward_label} / {conflictType.reverse_label}.</p>{/if}</div>{/if}
    {#snippet footer()}<Button label="Cancel" disabled={acting} onclick={closeDelete} /><Button tone="danger" surface="solid" label="Delete relationship type" disabled={acting} onclick={() => void removeRelationshipType()} />{/snippet}
  </Modal>
{/if}

<style>
  .relationships-tab, section, .refresh-error { display: grid; gap: var(--space-3); }
  header, .actions, .filters, .row-actions, .types li, .section-heading { display: flex; gap: var(--space-2); align-items: center; justify-content: space-between; flex-wrap: wrap; }
  h2, h3, p, ul { margin: 0; }
  header p, small, .muted { color: var(--text-muted); font-size: var(--font-size-sm); }
  .records, .types { list-style: none; padding: 0; display: grid; gap: var(--space-2); }
  .records li { display: flex; gap: var(--space-3); justify-content: space-between; align-items: start; flex-wrap: wrap; padding: var(--space-3); border: 1px solid var(--border-default); border-radius: var(--radius-sm); }
  .records li > div:first-child { display: grid; gap: var(--space-1); }
  .row-actions { justify-content: flex-start; }
  [role="alert"] { color: var(--text-danger); }
</style>
