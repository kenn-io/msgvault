<script lang="ts">
  import { formatPartialDate as dateText } from '../../directory/dates';
  import { appShortcuts, Button, Checkbox, Modal, IconButton } from '@kenn-io/kit-ui';
  import { RelationshipReviewController } from '../../directory/relationship-review-controller.svelte';
  import { untrack, onDestroy } from 'svelte';
  import Pencil from '@lucide/svelte/icons/pencil';
  import X from '@lucide/svelte/icons/x';

  import type { APIClient } from '../../api/client';
  import type { DirectoryEntityController } from '../../directory/entity-controller.svelte';
  import type { PersonRelationship, PersonRelationshipView, RelationshipType } from '../../directory/models';
  import PersonRelationshipEditor from './PersonRelationshipEditor.svelte';
  import DetailRow from '../people/DetailRow.svelte';
  import { sentenceCase } from '../../explore/labels';
  import { formatContactDate } from '../../directory/labels';
  import { historySource } from './history-format';
  import RelationshipTypeEditor from './RelationshipTypeEditor.svelte';

  interface Props {
    client: APIClient;
    controller: DirectoryEntityController;
    personID: number;
    personName?: string;
    catalogOnly?: boolean;
    onOpenPerson?: (id: number) => void;
  }

  let { client, controller, personID, personName, catalogOnly = false, onOpenPerson }: Props = $props();
  const imports = new RelationshipReviewController(untrack(() => client));
  $effect(() => { const id = personID; if (!catalogOnly && controller.relationships) untrack(() => imports.personID === id ? void imports.load() : imports.applyPerson(id)); });
  onDestroy(() => imports.destroy());
  let relationshipEditor = $state<PersonRelationshipView | null | undefined>(undefined);
  let relationshipTypeEditor = $state<RelationshipType | null | undefined>(undefined);
  let deletingRelationship = $state<PersonRelationshipView>();
  let deletingType = $state<RelationshipType>();
  let acting = $state(false);
  let actionMessage = $state('');
  let conflictRelationship = $state<PersonRelationship>();
  let conflictType = $state<RelationshipType>();
  $effect(() => {
    if (deletingRelationship || deletingType) return appShortcuts.pushScope('directory-relationships');
  });

  const pending = $derived(imports.rows.filter(row => row.status === 'pending'));
  const reviewed = $derived(imports.rows.filter(row => row.status !== 'pending'));

  const typeLabel = (type: RelationshipType) => type.is_symmetric ? type.forward_label : `${type.forward_label} / ${type.reverse_label}`;
  const byLabel = (left: RelationshipType, right: RelationshipType) => typeLabel(left).localeCompare(typeLabel(right));
  const customTypes = $derived(controller.relationshipTypes.filter((type) => type.ownership !== 'system').sort(byLabel));
  const builtInTypes = $derived(controller.relationshipTypes.filter((type) => type.ownership === 'system').sort(byLabel));

  // The editor picks from the type list, so a failed load retries when it opens.
  function openEditor(view: PersonRelationshipView | null): void {
    if (controller.errors.relationshipTypes) void controller.refreshRelationshipTypes();
    relationshipEditor = view;
  }

  function counterpart(view: PersonRelationshipView): string {
    return view.counterpart_display_name?.trim() || view.counterpart_vcard_uid || `Person ${view.counterpart_person_id}`;
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

<section class="relationships-tab" aria-label={catalogOnly ? 'Connection types' : 'Person relationships'}>
  {#if !catalogOnly}
    <header>
      <h3>Connections</h3>
      <div class="actions">
        <Checkbox checked={controller.relationshipsIncludeEnded} label="Show ended" ariaLabel="Include ended relationships" onchange={(checked) => void changeHistoryMode(checked)} />
        <Button size="sm" label="Add connection" onclick={() => openEditor(null)} />
      </div>
    </header>

    {#if controller.errors.relationships}
      <div class="refresh-error" role="alert">
        <p>{controller.errors.relationships}</p>
        <Button size="sm" label="Refresh relationships" disabled={controller.relationshipsLoading} onclick={() => void controller.refreshRelationships()} />
      </div>
    {/if}
    {#if controller.relationshipsLoading}<p class="muted" role="status">Loading relationships…</p>{/if}
    {#if controller.relationships.length === 0 && imports.rows.length === 0 && !controller.relationshipsLoading && !controller.errors.relationships}
      <p class="muted">No connections yet</p>
    {:else if controller.relationships.length > 0}
      <div class="records">
        {#each controller.relationships as view (view.relationship.id)}
          <DetailRow description={[sentenceCase(view.counterpart_label), view.relationship.start_date ? `since ${dateText(view.relationship.start_date)}` : '', view.relationship.end_date ? `ended ${dateText(view.relationship.end_date)}` : '', view.relationship.notes ?? ''].filter(Boolean).join(' · ')}>
            {#if onOpenPerson}<button class="person-link" type="button" onclick={() => onOpenPerson(view.counterpart_person_id)}>{counterpart(view)}</button>{:else}{counterpart(view)}{/if}
            {#snippet actions()}
              <IconButton size="sm" ariaLabel="Edit relationship" onclick={() => openEditor(view)}><Pencil size={14} /></IconButton>
              <IconButton size="sm" ariaLabel="Delete relationship" onclick={() => { deletingRelationship = view; }}><X size={14} /></IconButton>
            {/snippet}
          </DetailRow>
        {/each}
      </div>
    {/if}
    {#if imports.loading}<p role="status">Loading imported connections…</p>{/if}
    {#if imports.error}<p role="alert">{imports.error}<Button label="Retry imported connections" size="sm" onclick={() => void imports.retry()} /></p>{/if}
    {#snippet importedRow(row: typeof imports.rows[number])}
      <DetailRow label={row.status === 'pending' ? undefined : sentenceCase(row.status)} description={[row.raw_related_type, `from ${historySource(row.source)}`, sentenceCase(row.value_kind), row.updated_at && formatContactDate(row.updated_at), row.reviewed_at && formatContactDate(row.reviewed_at)].filter(Boolean).join(' · ')}>
        {row.raw_related_value}
        {#if row.matched_person_id && !controller.relationships.some(view => row.status === 'accepted' && view.counterpart_person_id === row.matched_person_id)}<Button label="Open person" size="sm" onclick={() => onOpenPerson?.(row.matched_person_id!)} />{/if}
      </DetailRow>
    {/snippet}
    {#if pending.length > 0}
      <section class="suggestions"><h4>Suggested ({pending.length})</h4>{#each pending as row (row.id)}{@render importedRow(row)}{/each}</section>
    {/if}
    {#if reviewed.length > 0}<section class="suggestions"><h4>Reviewed ({reviewed.length})</h4>{#each reviewed as row (row.id)}{@render importedRow(row)}{/each}</section>{/if}
  {:else}
    <header><h3>Connection types</h3><Button size="sm" label="Add type" onclick={() => relationshipTypeEditor = null} /></header>
    {#if controller.relationshipTypesLoading}<p class="muted" role="status">Loading relationship types…</p>
    {:else if controller.errors.relationshipTypes}<p role="alert">{controller.errors.relationshipTypes}<Button size="sm" label="Refresh relationship types" onclick={() => void controller.refreshRelationshipTypes()} /></p>
    {:else if controller.relationshipTypes.length === 0}<p class="muted">No relationship types are available.</p>
    {:else}
      {#if customTypes.length > 0}
        <ul class="types">
          {#each customTypes as type (type.id)}
            <li>
              <span class="type-name">{typeLabel(type)}</span>
              <Button size="sm" surface="soft" label="Edit" ariaLabel={`Edit ${type.slug} relationship type`} onclick={() => { relationshipTypeEditor = type; }} />
              {#if type.is_deletable}<Button size="sm" surface="soft" tone="danger" label="Delete" ariaLabel={`Delete ${type.slug} relationship type`} onclick={() => { deletingType = type; }} />{/if}
            </li>
          {/each}
        </ul>
      {/if}
      {#if builtInTypes.length > 0}
        <div class="built-in">
          <span data-section-label>Built in</span>
          <ul aria-label="Built-in connection types">{#each builtInTypes as type (type.id)}<li>{typeLabel(type)}</li>{/each}</ul>
        </div>
      {/if}
    {/if}
  {/if}
</section>

{#if relationshipEditor !== undefined}
  <PersonRelationshipEditor {client} {controller} {personID} {personName} relationship={relationshipEditor ?? undefined}
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
  .relationships-tab { display: grid; gap: var(--space-2); min-width: 0; }
  .refresh-error { display: flex; gap: var(--control-gap); align-items: center; }
  header, .actions { display: flex; gap: var(--control-gap); align-items: center; justify-content: space-between; flex-wrap: wrap; min-height: var(--control-h); }
  h3, p, ul { margin: 0; }
  h3 { font-size: var(--font-size-md); }
  .muted { color: var(--text-muted); font-size: var(--font-size-sm); }
  .suggestions { margin-top: var(--space-3); }
  h4 { margin: 0; font-size: var(--font-size-sm); }
  .records { display: grid; }
  .types { display: grid; padding: 0; border: 1px solid var(--border-muted); border-radius: var(--radius-md); list-style: none; }
  .types li { display: flex; align-items: center; gap: var(--control-gap); min-height: var(--control-h-lg); padding: var(--space-1) var(--pane-pad-x); }
  .types li + li { border-top: 1px solid var(--border-muted); }
  .type-name { flex: 1; min-width: 0; color: var(--text-primary); font-size: var(--font-size-sm); overflow-wrap: anywhere; }
  .built-in { display: grid; gap: var(--space-2); margin-top: var(--space-2); }
  .built-in ul { display: flex; flex-wrap: wrap; gap: var(--space-2); padding: 0; list-style: none; }
  .built-in li { padding: var(--space-1) var(--chip-pad-x); border: 1px solid var(--border-muted); border-radius: var(--radius-md); color: var(--text-secondary); font-size: var(--font-size-sm); }
  [role="alert"] { color: var(--text-danger); }
  .person-link { padding: 0; border: 0; background: none; color: var(--link-ink); font: inherit; cursor: pointer; }
  .person-link:hover { text-decoration: underline; }
</style>
