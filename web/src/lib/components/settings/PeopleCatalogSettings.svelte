<script lang="ts">
  import { Button, Checkbox, Modal, SearchInput, TextInput } from '@kenn-io/kit-ui';
  import { onDestroy, onMount, untrack } from 'svelte';
  import type { APIClient } from '../../api/client';
  import type { AttributeDefinition, TargetDescriptor } from '../../api/generated/models';
  import { listPersonFactTargets, getAttributeDefinition, patchAttributeDefinition, deleteAttributeDefinition } from '../../api/generated/api/api';
  import { DirectoryEntityController } from '../../directory/entity-controller.svelte';
  import { DirectoryProfileController } from '../../directory/profile-controller.svelte';
  import RelationshipsTab from '../directory/RelationshipsTab.svelte';
  import AttributeDefinitionDialog from '../directory/AttributeDefinitionDialog.svelte';
  import OrganizationsSettings from './OrganizationsSettings.svelte';
  import { valueTypeOptions } from '../../directory/field-options';
  import InlineNotice from '../common/InlineNotice.svelte';
  let { client, onDirectoryChange, onOpenPerson }: { client: APIClient; onDirectoryChange?: () => void | Promise<void>; onOpenPerson?: (personID: number) => void } = $props();
  const entities = new DirectoryEntityController(untrack(() => client), 0, { onDirectoryChange: () => onDirectoryChange?.() });
  const fields = new DirectoryProfileController(untrack(() => client), 0, { etags: {}, errors: {} });
  let query = $state('');
  let targets = $state<TargetDescriptor[]>([]);
  let catalogError = $state(false);
  let targetsLoading = $state(false);
  let loading = $state(true);
  const catalogAbort = new AbortController();
  const matching = (name: string, description = '') => `${name} ${description}`.toLowerCase().includes(query.trim().toLowerCase());
  const visibleFields = $derived([...fields.definitions].sort((a, b) => a.display_order - b.display_order || a.label.localeCompare(b.label)).filter(field => matching(field.label, field.description ?? '')));
  const work = $derived(targets.find(target => target.kind === 'employment'));
  const visibleWork = $derived(work && matching('Work', work.description) ? work : undefined);
  async function loadTargets(): Promise<void> {
    if (targetsLoading) return;
    targetsLoading = true;
    catalogError = false;
    try {
      const response = await listPersonFactTargets({ include_sensitive: true }, { ...client, signal: catalogAbort.signal });
      if (catalogAbort.signal.aborted) return;
      if (!response.data) { catalogError = true; return; }
      targets = response.data.targets;
    } catch { if (!catalogAbort.signal.aborted) catalogError = true; }
    finally { targetsLoading = false; }
  }
  let creating = $state(false);
  let editing = $state<AttributeDefinition>();
  let deleting = $state(false);
  let etag = $state('');
  let label = $state('');
  let description = $state('');
  let sensitive = $state(false);
  let active = $state(true);
  let pending = $state(false);
  let error = $state('');
  onMount(() => { void Promise.all([fields.reloadDefinitions(), loadTargets()]).finally(() => loading = false); void entities.refreshRelationshipTypes(); });
  onDestroy(() => { catalogAbort.abort(); fields.destroy(); entities.destroy(); });
  async function edit(field: AttributeDefinition): Promise<void> {
    if (pending) return;
    pending = true; error = '';
    try {
      const response = await getAttributeDefinition({ id: field.id }, client);
      if (!response.data || !response.response.headers.get('ETag')) throw new Error(response.error?.message ?? 'Could not load field.');
      editing = response.data; etag = response.response.headers.get('ETag')!;
      label = editing.label; description = editing.description ?? ''; sensitive = editing.is_sensitive; active = editing.is_active;
      deleting = false;
    } catch (cause) { error = cause instanceof Error ? cause.message : 'Could not load field.'; }
    finally { pending = false; }
  }
  async function save(remove = false): Promise<void> {
    if (!editing || pending) return;
    pending = true; error = '';
    try {
      const options = { ...client, headers: { 'If-Match': etag } };
      const response = remove ? await deleteAttributeDefinition({ id: editing.id }, options)
        : await patchAttributeDefinition({ id: editing.id }, { label: label.trim(), description, is_sensitive: sensitive, is_active: active }, options);
      if (!response.response.ok) throw new Error(response.error?.message ?? 'Could not save field.');
      editing = undefined;
      await fields.reloadDefinitions();
    } catch (cause) { error = cause instanceof Error ? cause.message : 'Could not save field.'; }
    finally { pending = false; }
  }
</script>

<section class="fields" aria-label="Fields">
  <header><h3>Fields</h3><Button label="Add field" size="sm" disabled={pending || fields.definitionCreationCommit !== null || fields.createdDefinition !== null} onclick={() => creating = true} /></header>
  {#if fields.definitionCreationCommit || fields.createdDefinition}<Button label="Resume field creation" onclick={() => creating = true} />{/if}
  {#if fields.definitionsError}<p role="alert">{fields.definitionsError}<Button label="Retry fields" onclick={() => void fields.reloadDefinitions()} /></p>{/if}
  {#if error && !editing}<p role="alert">{error}</p>{/if}
  <SearchInput value={query} ariaLabel="Search fields" placeholder="Search fields" block oninput={value => query = value} />
  {#if catalogError}<InlineNotice message="Couldn't load Work details." actionLabel="Retry" onaction={() => void loadTargets()} />{/if}
  {#if visibleFields.length || visibleWork}
    <ul class="list">
      {#if visibleWork}<li><span class="name">Work<small>{visibleWork.description}</small></span><small>Jobs{visibleWork.cardinality === 'multi' ? ' · several values' : ''}{visibleWork.sensitive ? ' · Sensitive' : ''}</small></li>{/if}
      {#each visibleFields as field (field.id)}
        <li><span class="name">{field.label}<small>{field.description ?? ''}</small></span><small>{['Profile field', valueTypeOptions.find(option => option.value === field.value_type)?.label ?? 'Other', field.cardinality === 'multi' ? 'several values' : '', field.is_sensitive ? 'Sensitive' : '', field.is_active ? '' : 'Inactive'].filter(Boolean).join(' · ')}</small>{#if field.ownership !== 'system'}<Button label="Edit" ariaLabel={`Edit ${field.label}`} size="sm" surface="soft" disabled={pending} onclick={() => void edit(field)} />{/if}</li>
      {/each}
    </ul>
  {:else if loading || targetsLoading}<p class="muted" role="status">Loading fields…</p>
  {:else if !fields.definitionsError && !catalogError}<p class="muted">{query.trim() ? 'No fields match' : 'No fields yet'}</p>{/if}
</section>
<OrganizationsSettings controller={entities} {onOpenPerson} />
<RelationshipsTab {client} controller={entities} personID={0} catalogOnly />
{#if creating}<AttributeDefinitionDialog controller={fields} onClose={() => creating = false} />{/if}
{#if editing}
  <Modal title={deleting ? `Delete ${editing.label}?` : 'Edit field'} onclose={() => { if (!pending) editing = undefined; }}>
    {#if !deleting}
      <div class="form"><label>Name<TextInput bind:value={label} ariaLabel="Name" block disabled={pending} /></label><label>Description<TextInput bind:value={description} ariaLabel="Description" block disabled={pending} /></label><Checkbox label="Sensitive" bind:checked={sensitive} disabled={pending} /><Checkbox label="Active" bind:checked={active} disabled={pending} /></div>
    {/if}
    {#if error}<p role="alert">{error}</p><Button label="Reload field" disabled={pending} onclick={() => editing && void edit(editing)} />{/if}
    {#snippet footer()}
      <Button label="Cancel" disabled={pending} onclick={() => editing = undefined} />
      {#if editing?.is_deletable && !deleting}<Button label="Delete" tone="danger" disabled={pending} onclick={() => deleting = true} />{/if}
      <Button label={deleting ? 'Confirm delete field' : 'Save changes'} disabled={pending || !label.trim()} onclick={() => void save(deleting)} />
    {/snippet}
  </Modal>
{/if}
<style>
  .fields, .form { display: grid; gap: var(--field-gap); }
  header { display: flex; align-items: center; gap: var(--control-gap); }
  header { justify-content: space-between; }
  h3, p { margin: 0; }
  h3 { font-size: var(--font-size-md); }
  /* Rows share one set of columns, so the kind column ends at the same place whether or not the row has Edit.
   * The last column keeps room for Edit (its side padding and a short label) and the row's inset even when no visible row has one. */
  .list { display: grid; grid-template-columns: minmax(0, 1fr) fit-content(50%) minmax(calc(2 * var(--control-pad-x) + var(--space-8) + var(--pane-pad-x)), auto); column-gap: var(--control-gap); margin: 0; padding: 0; border: 1px solid var(--border-muted); border-radius: var(--radius-md); list-style: none; }
  .list li { display: grid; grid-column: 1 / -1; grid-template-columns: subgrid; align-items: center; min-height: var(--person-list-item-h); box-sizing: border-box; padding: var(--space-1) var(--pane-pad-x); }
  .list li + li { border-top: 1px solid var(--border-muted); }
  /* Each field's name (Body) over its description (Meta); its kind and value type (Meta) at the right. */
  .list span { min-width: 0; font-size: var(--type-body-size); line-height: var(--type-body-line); overflow-wrap: anywhere; }
  .list li > small { justify-self: end; text-align: end; }
  .name > small { display: block; }
  small, .muted { color: var(--text-muted); font-size: var(--type-meta-size); line-height: var(--type-meta-line); }
  label { display: grid; gap: var(--label-gap); }
  [role="alert"] { color: var(--text-danger); }
  @media (max-width: 600px) {
    .list { display: block; }
    .list li { grid-template-columns: minmax(0, 1fr) auto; grid-template-rows: minmax(var(--control-h), auto) auto auto; row-gap: var(--space-1); padding-block: var(--pane-pad-x); }
    .name { display: contents; }
    .name > small { grid-column: 1 / -1; grid-row: 2; }
    .list li > small { grid-column: 1 / -1; grid-row: 3; justify-self: start; text-align: start; }
    .list li > :global(button) { grid-column: 2; grid-row: 1; }
  }
</style>
