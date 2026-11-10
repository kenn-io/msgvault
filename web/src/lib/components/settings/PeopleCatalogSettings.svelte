<script lang="ts">
  import { Button, Checkbox, Modal, TextInput } from '@kenn-io/kit-ui';
  import { onDestroy, onMount, untrack } from 'svelte';
  import type { APIClient } from '../../api/client';
  import type { AttributeDefinition } from '../../api/generated/models';
  import { getAttributeDefinition, patchAttributeDefinition, deleteAttributeDefinition } from '../../api/generated/api/api';
  import { DirectoryEntityController } from '../../directory/entity-controller.svelte';
  import { DirectoryProfileController } from '../../directory/profile-controller.svelte';
  import RelationshipsTab from '../directory/RelationshipsTab.svelte';
  import AttributeDefinitionDialog from '../directory/AttributeDefinitionDialog.svelte';
  import OrganizationsSettings from './OrganizationsSettings.svelte';
  import { sentenceCase } from '../../explore/labels';
  let { client, onDirectoryChange, onOpenPerson }: { client: APIClient; onDirectoryChange?: () => void | Promise<void>; onOpenPerson?: (personID: number) => void } = $props();
  const entities = new DirectoryEntityController(untrack(() => client), 0, { onDirectoryChange: () => onDirectoryChange?.() });
  const fields = new DirectoryProfileController(untrack(() => client), 0, { etags: {}, errors: {} });
  const customFields = $derived(fields.definitions.filter((field) => field.ownership !== 'system'));
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
  onMount(() => { void fields.reloadDefinitions(); void entities.refreshRelationshipTypes(); });
  onDestroy(() => { fields.destroy(); entities.destroy(); });
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
  {#if customFields.length > 0}
    <ul class="list">
      {#each customFields as field (field.id)}
        <li><span>{field.label}</span><small>{[sentenceCase(field.value_type), field.cardinality === 'multi' ? 'several values' : '', field.is_active ? '' : 'inactive'].filter(Boolean).join(' · ')}</small><Button label="Edit" ariaLabel={`Edit ${field.label}`} size="sm" surface="soft" disabled={pending} onclick={() => void edit(field)} /></li>
      {/each}
    </ul>
  {:else}<p class="muted">No custom fields yet</p>{/if}
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
  .list { display: grid; margin: 0; padding: 0; border: 1px solid var(--border-muted); border-radius: var(--radius-md); list-style: none; }
  .list li { display: flex; align-items: center; gap: var(--control-gap); min-height: var(--control-h-lg); padding: var(--space-1) var(--pane-pad-x); }
  .list li + li { border-top: 1px solid var(--border-muted); }
  .list span { flex: 1; min-width: 0; font-size: var(--font-size-sm); overflow-wrap: anywhere; }
  small, .muted { color: var(--text-muted); font-size: var(--font-size-sm); }
  label { display: grid; gap: var(--label-gap); }
  [role="alert"] { color: var(--text-danger); }
</style>
