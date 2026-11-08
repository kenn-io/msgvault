<script lang="ts">
  import { conflict, connectionFacts } from '../../directory/conflict';
  import { WriteAttempt } from '../../directory/write-attempt.svelte';
  import { latestDateKey } from '../../directory/dates';
  import { appShortcuts, Button } from '@kenn-io/kit-ui';
  import { RelationshipReviewController } from '../../directory/relationship-review-controller.svelte';
  import { untrack, onDestroy } from 'svelte';
  import DetailSection from '../people/DetailSection.svelte';
  import { Disclosure } from '@kenn-io/kit-ui';
  import EmptyLine from '../common/EmptyLine.svelte';
  import InlineNotice from '../common/InlineNotice.svelte';
  import Dialog from '../common/Dialog.svelte';

  import type { APIClient } from '../../api/client';
  import type { DirectoryEntityController } from '../../directory/entity-controller.svelte';
  import type { PersonRelationship, PersonRelationshipView, Prepared, RelationshipType } from '../../directory/models';
  import PersonRelationshipEditor from './PersonRelationshipEditor.svelte';
  import DetailRow from '../people/DetailRow.svelte';
  import { sentenceCase } from '../../explore/labels';
  import { dateSpan } from '../../directory/labels';
  import { provenance, type ProvenanceLine } from './history-format';
  import ConflictNotice from '../common/ConflictNotice.svelte';
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
  const removal = new WriteAttempt<Prepared<PersonRelationship>>();
  const typeRemoval = new WriteAttempt<Prepared<RelationshipType>>();
  const acting = $derived(removal.busy || typeRemoval.busy);
  $effect(() => {
    if (deletingRelationship || deletingType) return appShortcuts.pushScope('directory-relationships');
  });

  const currentConnections = $derived(controller.relationships.filter(view => view.relationship.status !== 'ended'));
  const endedConnections = $derived(controller.relationships.filter(view => view.relationship.status === 'ended').sort((a, b) => latestDateKey(b.relationship.end_date) - latestDateKey(a.relationship.end_date)));
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

  // "Text · Friend · from a contact file · Nov 1": whether the value is text or a reference (a contact's reference shows
  // itself), the imported type, then who imported it and when. The review date sits in the tooltip.
  function importedLine(row: (typeof imports.rows)[number]): ProvenanceLine {
    const line = provenance(row.source, row.updated_at ?? row.reviewed_at ?? '', undefined, { times: [['Reviewed', row.reviewed_at ?? undefined]] });
    const kind = row.raw_related_value.startsWith('urn:uuid:') ? row.raw_related_value : row.value_kind === 'uri' ? 'Reference' : 'Text';
    const lead = [kind, sentenceCase(row.raw_related_type)].filter(Boolean).join(' · ');
    return { text: lead ? `${lead} · ${line.text.charAt(0).toLowerCase()}${line.text.slice(1)}` : line.text, title: line.title };
  }

  function counterpart(view: PersonRelationshipView): string {
    return view.counterpart_display_name?.trim() || 'Unnamed person';
  }

  async function removeRelationship(): Promise<void> {
    if (!deletingRelationship || acting || !removal.prepared) return;
    const target = deletingRelationship.relationship;
    if (await removal.run(current => controller.deleteRelationship(target.id, current))) deletingRelationship = undefined;
  }

  async function prepareDelete(): Promise<void> {
    if (!deletingRelationship) return;
    await removal.prepare(() => controller.prepareRelationshipMutation(deletingRelationship!.relationship.id));
    if (removal.prepared && deletingRelationship) deletingRelationship = { ...deletingRelationship, relationship: removal.prepared.data };
  }

  function openDelete(view: PersonRelationshipView): void {
    removal.reset();
    deletingRelationship = view;
    void prepareDelete();
  }

  async function removeRelationshipType(): Promise<void> {
    if (!deletingType || acting || !typeRemoval.prepared) return;
    if (await typeRemoval.run(current => controller.deleteRelationshipType(deletingType!.id, current))) deletingType = undefined;
  }

  async function prepareTypeDelete(): Promise<void> {
    if (!deletingType) return;
    await typeRemoval.prepare(() => controller.prepareRelationshipTypeMutation(deletingType!.id));
    if (typeRemoval.prepared) deletingType = typeRemoval.prepared.data;
  }

  function openTypeDelete(type: RelationshipType): void {
    typeRemoval.reset();
    deletingType = type;
    void prepareTypeDelete();
  }

  function closeDelete(): void {
    if (acting) return;
    if (removal.conflict) void controller.refreshRelationships();
    if (typeRemoval.conflict) void controller.refreshRelationshipTypes();
    deletingRelationship = undefined;
    deletingType = undefined;
    removal.reset();
    typeRemoval.reset();
  }
</script>

<section class="relationships-tab" aria-label={catalogOnly ? 'Connection types' : undefined}>
  {#if !catalogOnly}
    <DetailSection title="Connections" addLabel="Add connection…" onadd={() => openEditor(null)}
      history={endedConnections.length ? endedRows : undefined} historyLabel={`${endedConnections.length} past ${endedConnections.length === 1 ? 'connection' : 'connections'}`}
      subgroup={imports.rows.length || imports.loading || imports.error ? addressBookRows : undefined} subgroupTitle="From address books">
      {#if controller.relationshipsLoading}<p role="status">Loading connections…</p>{/if}
      {#if !controller.relationships.length && !imports.rows.length && !controller.relationshipsLoading && !controller.errors.relationships}<EmptyLine message="No connections yet" />{/if}
      <div class="records">{#each currentConnections as view (view.relationship.id)}{@render connectionRow(view)}{/each}</div>
      {#if controller.errors.relationships && relationshipEditor === undefined}<InlineNotice message="Couldn't load connections." details={controller.errors.relationships} actionLabel="Retry" onaction={() => void controller.refreshRelationships()} />{/if}
    </DetailSection>
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
              {#if type.is_deletable}<Button size="sm" surface="soft" tone="danger" label="Delete" ariaLabel={`Delete ${type.slug} relationship type`} onclick={() => openTypeDelete(type)} />{/if}
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
  <Dialog title={`Delete connection with ${counterpart(deletingRelationship)}?`} width="small" primaryLabel="Delete" primaryTone="danger" primaryDisabled={!removal.prepared} onclose={closeDelete} onprimary={() => void removeRelationship()} busy={acting}>
    <p class="confirm">This permanently removes the connection.</p>
    {#snippet notice()}
      {#if removal.conflict && deletingRelationship}
        <!-- Delete again removes the saved version the notice shows. -->
        <ConflictNotice noun="connection" saved={removal.conflict.data} {...conflict(connectionFacts(removal.conflict.data), connectionFacts(deletingRelationship.relationship), connectionFacts(deletingRelationship.relationship))} message={removal.details} />
      {:else if removal.details}<InlineNotice message="Couldn't delete this connection." details={removal.details} actionLabel="Retry" onaction={() => { if (removal.prepared) void removeRelationship(); else void prepareDelete(); }} />{/if}
    {/snippet}
  </Dialog>
{/if}
{#if deletingType}
  <Dialog title={`Delete ${deletingType.slug} connection type?`} width="small" primaryLabel="Delete" primaryTone="danger" primaryDisabled={!typeRemoval.prepared || !typeRemoval.prepared.data.is_deletable || typeRemoval.prepared.data.ownership === 'system'} onclose={closeDelete} onprimary={() => void removeRelationshipType()} busy={acting}>
    <p class="confirm">This permanently removes the connection type.</p>
    {#snippet notice()}{#if typeRemoval.failed || typeRemoval.details}<InlineNotice message={typeRemoval.conflict ? 'This connection type changed since you opened it.' : "Couldn't delete this connection type."} details={typeRemoval.details} actionLabel={typeRemoval.conflict ? 'Reload' : 'Retry'} onaction={() => { if (typeRemoval.conflict) { typeRemoval.reset(); void prepareTypeDelete(); } else if (typeRemoval.prepared) void removeRelationshipType(); else void prepareTypeDelete(); }} />{/if}{/snippet}
  </Dialog>
{/if}

{#snippet endedRows()}<div class="records">{#each endedConnections as view (view.relationship.id)}{@render connectionRow(view)}{/each}</div>{/snippet}
{#snippet addressBookRows()}
  {#if imports.loading}<p role="status">Loading imported connections…</p>{/if}
  {#if pending.length}<h5>Suggested ({pending.length})</h5><div class="records">{#each pending as row (row.id)}{@render importedRow(row)}{/each}</div>{/if}
  {#if reviewed.length}<Disclosure variant="plain" class="fold" title={`${reviewed.length} reviewed`}><div class="records">{#each reviewed as row (row.id)}{@render importedRow(row)}{/each}</div></Disclosure>{/if}
  {#if imports.error}<InlineNotice message={imports.error} actionLabel="Retry" onaction={() => void imports.retry()} />{/if}
{/snippet}
{#snippet connectionRow(view: PersonRelationshipView)}
  <!-- The person is a link in the row's text, so the row itself takes no tap: Edit… is in ⋯. -->
  <DetailRow actionLabel={counterpart(view)} description={[sentenceCase(view.counterpart_label), dateSpan(view.relationship.start_date, view.relationship.end_date, view.relationship.status !== 'ended', { later: true }), view.relationship.notes ?? ''].filter(Boolean).join(' · ')} menu={[
    { label: 'Edit…', onselect: () => openEditor(view) },
    { label: 'Delete…', danger: true, onselect: () => openDelete(view) }
  ]}>
    {#if onOpenPerson}<button data-link type="button" onclick={() => onOpenPerson(view.counterpart_person_id)}>{counterpart(view)}</button>{:else}{counterpart(view)}{/if}
  </DetailRow>
{/snippet}
{#snippet importedRow(row: typeof imports.rows[number])}
  {@const reference = row.raw_related_value.startsWith('urn:uuid:')}
  <DetailRow onclick={row.matched_person_id && onOpenPerson ? () => onOpenPerson(row.matched_person_id!) : undefined} actionLabel="Open matched person" description={importedLine(row)} exception={row.status !== 'pending' ? sentenceCase(row.status) : undefined}>
    {reference ? (row.matched_person_id ? 'Matched contact' : 'Unknown contact') : row.raw_related_value}
  </DetailRow>
{/snippet}

<style>
  .relationships-tab { display: grid; min-width: 0; }
  header { display: flex; gap: var(--control-gap); align-items: center; justify-content: space-between; flex-wrap: wrap; min-height: var(--control-h); }
  h3, p, ul { margin: 0; }
  h3 { font-size: var(--font-size-md); }
  .confirm { font-size: var(--type-body-size); line-height: var(--type-body-line); }
  .muted { color: var(--text-muted); font-size: var(--font-size-sm); }
  /* "Suggested (2)": a group label (Small) 8 above its rows. */
  h5 { margin: 0 0 var(--space-group); color: var(--text-muted); font-size: var(--type-small-size); font-weight: 500; line-height: var(--type-small-line); }
  [role="status"] { padding-block: var(--row-pad-y); color: var(--text-muted); font-size: var(--type-meta-size); line-height: var(--type-meta-line); }
  .records { display: grid; }
  .types { display: grid; padding: 0; border: 1px solid var(--border-muted); border-radius: var(--radius-md); list-style: none; }
  .types li { display: flex; align-items: center; gap: var(--control-gap); min-height: var(--control-h-lg); padding: var(--space-1) var(--pane-pad-x); }
  .types li + li { border-top: 1px solid var(--border-muted); }
  .type-name { flex: 1; min-width: 0; color: var(--text-primary); font-size: var(--font-size-sm); overflow-wrap: anywhere; }
  .built-in { display: grid; gap: var(--space-2); margin-top: var(--space-2); }
  .built-in ul { display: flex; flex-wrap: wrap; gap: var(--space-2); padding: 0; list-style: none; }
  .built-in li { padding: var(--space-1) var(--chip-pad-x); border: 1px solid var(--border-muted); border-radius: var(--radius-md); color: var(--text-secondary); font-size: var(--font-size-sm); }
</style>
