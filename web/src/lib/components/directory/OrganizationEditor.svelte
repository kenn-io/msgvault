<script lang="ts">
  import { appShortcuts, Button, Modal, SelectDropdown, TextInput } from '@kenn-io/kit-ui';
  import { onDestroy, onMount, untrack } from 'svelte';

  import type { DirectoryEntityController } from '../../directory/entity-controller.svelte';
  import { organizationKindOptions as kindOptions } from '../../directory/labels';
  import type { DirectoryEntityMutationResult, Organization, OrganizationProfile, OrganizationProfileBody } from '../../directory/models';

  import { historyProvenance } from './history-format';
  import { formatDateTime } from '../../util/format';
  import { sentenceCase } from '../../explore/labels';

  interface Props {
    controller: DirectoryEntityController;
    organization?: Pick<Organization, 'id'> & Partial<Organization>;
    onDone?: (organization?: Organization) => void;
    onClose?: () => void;
  }

  let { controller, organization = undefined, onDone = () => undefined, onClose = () => undefined }: Props = $props();
  const initialOrganization = untrack(() => organization);
  let name = $state(initialOrganization?.name ?? '');
  let kind = $state(initialOrganization?.kind ?? 'company');
  let primaryDomain = $state(initialOrganization?.primary_domain ?? '');
  let description = $state(initialOrganization?.description ?? '');
  let aliases = $state('');
  let categories = $state('');
  let baselineAliases = $state<string[]>([]);
  let baselineCategories = $state<string[]>([]);
  let currentProfile = $state<OrganizationProfile>();
  let loading = $state(!!initialOrganization);
  let submitting = $state(false);
  let message = $state('');
  let conflictCurrent = $state<OrganizationProfile>();
  let confirmDelete = $state(false);
  let checkingDelete = $state(false);
  let deleteError = $state('');
  let holders = $state<Awaited<ReturnType<DirectoryEntityController['organizationEmploymentHolders']>>>();
  const saveDisabled = $derived(loading || !name.trim() || controller.createBlocked.organizations);
  let releaseScope: (() => void) | undefined;

  onMount(() => {
    releaseScope = appShortcuts.pushScope('directory-organization-editor');
    if (initialOrganization) void loadCurrent();
  });
  onDestroy(() => releaseScope?.());

  async function loadCurrent(): Promise<void> {
    if (!initialOrganization) return;
    loading = true;
    message = '';
    try {
      const profile = await controller.prepareOrganizationMutation(initialOrganization.id);
      currentProfile = profile;
      name = profile.organization.name;
      kind = profile.organization.kind;
      primaryDomain = profile.organization.primary_domain ?? '';
      description = profile.organization.description ?? '';
      baselineAliases = (profile.names ?? []).filter((item) => item.name_kind === 'alias').map((item) => item.name);
      baselineCategories = (profile.categories ?? []).map((item) => item.category);
      aliases = baselineAliases.join(', ');
      categories = baselineCategories.join(', ');
    } catch (cause: unknown) {
      message = cause instanceof Error ? cause.message : 'Unable to load the organization.';
    } finally {
      loading = false;
    }
  }

  async function saveOrganization(): Promise<void> {
    if (loading || confirmDelete || submitting || !name.trim() || controller.createBlocked.organizations) return;
    submitting = true;
    message = '';
    conflictCurrent = undefined;
    try {
      const body = { name: name.trim(), kind: kind as 'company', primary_domain: primaryDomain.trim() || null, description: description.trim() || null };
      let saved = currentProfile?.organization;
      if (!saved || Object.entries(body).some(([key, value]) => value !== (saved?.[key as keyof Organization] ?? null))) {
        const result = initialOrganization || saved
          ? await controller.updateOrganization((initialOrganization ?? saved)!.id, body)
          : await controller.createOrganization(body);
        if (!result.ok) { handleFailure(result); return; }
        saved = result.entity!;
        currentProfile = { names: [], categories: [], contact_points: [], addresses: [], identifiers: [], media: [], ...currentProfile, organization: saved };
      }
      const aliasDraft = aliases === baselineAliases.join(', ') ? baselineAliases : values(aliases);
      const categoryDraft = categories === baselineCategories.join(', ') ? baselineCategories : values(categories);
      const aliasDelta = valueDelta(baselineAliases, aliasDraft);
      const categoryDelta = valueDelta(baselineCategories, categoryDraft);
      if (aliasDelta.added.length || aliasDelta.removed.size || categoryDelta.added.length || categoryDelta.removed.size) {
        const result = await controller.putOrganizationProfile(saved!.id, (freshProfile) => profileBody(freshProfile, aliasDelta, categoryDelta));
        if (!result.ok) {
          handleFailure(result);
          message = `Aliases and categories remain unsaved. ${message}`;
          return;
        }
      }
      onDone(saved);
    } finally {
      submitting = false;
    }
  }

  async function checkDeletion(clearMessage = true): Promise<void> {
    if (!initialOrganization) return;
    confirmDelete = true;
    checkingDelete = true;
    deleteError = '';
    holders = undefined;
    if (clearMessage) message = '';
    try {
      holders = await controller.organizationEmploymentHolders(initialOrganization.id);
    } catch (cause) {
      deleteError = cause instanceof Error ? cause.message : 'Unable to check employment records.';
    } finally {
      checkingDelete = false;
    }
  }

  async function remove(): Promise<void> {
    if (!initialOrganization || submitting) return;
    submitting = true;
    message = '';
    try {
      const result = await controller.deleteOrganization(initialOrganization.id);
      if (result.ok) { onDone(); return; }
      handleFailure(result);
      await checkDeletion(false);
    } finally {
      submitting = false;
    }
  }

  function handleFailure(result: Exclude<DirectoryEntityMutationResult<unknown, Organization | OrganizationProfile>, { ok: true }>): void {
    if (result.kind === 'conflict') {
      conflictCurrent = asProfile(result.current);
      message = 'This organization changed elsewhere.';
    } else if (result.kind === 'unknown' || result.kind === 'blocked') {
      message = "We couldn't confirm it was saved. Refresh organizations before trying again.";
    } else {
      message = result.message;
    }
  }

  function asProfile(current: Organization | OrganizationProfile | undefined): OrganizationProfile | undefined {
    return current && 'organization' in current ? current as OrganizationProfile : undefined;
  }

  async function refreshAfterUnknown(): Promise<void> {
    await controller.refreshOrganizations();
    if (!controller.createBlocked.organizations) message = '';
  }

  function requestClose(): void {
    if (!submitting) onClose();
  }

  function values(value: string): string[] {
    return value.split(',').map((item) => item.trim()).filter(Boolean);
  }

  function present(parts: Array<string | number | undefined>): string {
    return parts.filter((part) => part !== undefined && part !== '').join(' · ');
  }

  type Name = NonNullable<OrganizationProfile['names']>[number];
  type NameBody = NonNullable<OrganizationProfileBody['names']>[number];
  type Category = NonNullable<OrganizationProfile['categories']>[number];
  type CategoryBody = NonNullable<OrganizationProfileBody['categories']>[number];
  type EnvelopeBody = Omit<NameBody, 'name' | 'name_kind'>;
  interface ValueDelta {
    added: string[];
    removed: Set<string>;
  }

  function valueDelta(baseline: string[], draft: string[]): ValueDelta {
    const baselineValues = new Set(baseline);
    const draftValues = new Set(draft);
    return {
      added: draft.filter((value) => !baselineValues.has(value)),
      removed: new Set(baseline.filter((value) => !draftValues.has(value)))
    };
  }

  function envelopeBody(envelope: Name['envelope']): EnvelopeBody {
    return {
      ordinal: envelope.ordinal,
      source: envelope.source as NameBody['source'],
      ...(envelope.pref === undefined ? {} : { pref: envelope.pref }),
      ...(envelope.type_label === undefined ? {} : { type_label: envelope.type_label }),
      ...(envelope.type_tokens === undefined ? {} : { type_tokens: envelope.type_tokens }),
      ...(envelope.source_ref === undefined ? {} : { source_ref: envelope.source_ref }),
      ...(envelope.source_resource_uid === undefined ? {} : { source_resource_uid: envelope.source_resource_uid }),
      ...(envelope.confidence === undefined ? {} : { confidence: envelope.confidence }),
      ...(envelope.active_from === undefined ? {} : { active_from: envelope.active_from }),
      ...(envelope.vcard.property === undefined ? {} : { vcard_property: envelope.vcard.property }),
      ...(envelope.vcard.group === undefined ? {} : { vcard_group: envelope.vcard.group }),
      ...(envelope.vcard.prop_id === undefined ? {} : { vcard_prop_id: envelope.vcard.prop_id }),
      ...(envelope.vcard.pid === undefined ? {} : { vcard_pid: envelope.vcard.pid }),
      ...(envelope.vcard.altid === undefined ? {} : { vcard_altid: envelope.vcard.altid })
    };
  }

  function namesWithDelta(profile: OrganizationProfile, delta: ValueDelta): NameBody[] {
    const retained = (profile.names ?? []).filter((item) => item.name_kind !== 'alias' || !delta.removed.has(item.name)).map(nameBody);
    const freshAliases = new Set((profile.names ?? []).filter((item) => item.name_kind === 'alias').map((item) => item.name));
    const nextOrdinal = Math.max(-1, ...(profile.names ?? []).map((item) => item.envelope.ordinal)) + 1;
    return [...retained, ...delta.added.filter((name) => !freshAliases.has(name)).map((name, index) => {
      const created: NameBody = { name, name_kind: 'alias', ordinal: nextOrdinal + index, source: 'user' };
      return created;
    })];
  }

  function nameBody(item: Name): NameBody {
    return { name: item.name, name_kind: item.name_kind as NameBody['name_kind'], ...envelopeBody(item.envelope) };
  }

  function categoriesWithDelta(profile: OrganizationProfile, delta: ValueDelta): CategoryBody[] {
    const retained = (profile.categories ?? []).filter((item) => !delta.removed.has(item.category)).map(categoryBody);
    const freshCategories = new Set((profile.categories ?? []).map((item) => item.category));
    const nextOrdinal = Math.max(-1, ...(profile.categories ?? []).map((item) => item.envelope.ordinal)) + 1;
    return [...retained, ...delta.added.filter((category) => !freshCategories.has(category)).map((category, index) => ({
      category, ordinal: nextOrdinal + index, source: 'user' as const
    }))];
  }

  function categoryBody(item: Category): CategoryBody {
    return { category: item.category, ...envelopeBody(item.envelope) };
  }

  function profileBody(
    profile: OrganizationProfile,
    aliasDelta: ValueDelta,
    categoryDelta: ValueDelta
  ): OrganizationProfileBody {
    return {
      names: namesWithDelta(profile, aliasDelta),
      categories: categoriesWithDelta(profile, categoryDelta),
      contact_points: (profile.contact_points ?? []).map((item) => ({
        contact_kind: item.address_kind as NonNullable<OrganizationProfileBody['contact_points']>[number]['contact_kind'], original_value: item.original_value,
        ...envelopeBody(item.envelope),
        ...(item.service_slug === undefined ? {} : { service_slug: item.service_slug }), ...(item.scope_kind === undefined ? {} : { scope_kind: item.scope_kind }),
        ...(item.scope_value === undefined ? {} : { scope_value: item.scope_value }), ...(item.uri === undefined ? {} : { uri: item.uri })
      })),
      addresses: (profile.addresses ?? []).map((item) => ({
        address_kind: item.address_kind, original_value: item.original_value, ...envelopeBody(item.envelope),
        ...(item.post_office_box === undefined ? {} : { post_office_box: item.post_office_box }),
        ...(item.extended_address === undefined ? {} : { extended_address: item.extended_address }),
        ...(item.street_address === undefined ? {} : { street_address: item.street_address }), ...(item.locality === undefined ? {} : { locality: item.locality }),
        ...(item.region === undefined ? {} : { region: item.region }), ...(item.postal_code === undefined ? {} : { postal_code: item.postal_code }),
        ...(item.country_name === undefined ? {} : { country_name: item.country_name }),
        ...(item.extended_components === undefined ? {} : { extended_components: item.extended_components }),
        ...(item.free_text === undefined ? {} : { free_text: item.free_text }), ...(item.place_uri === undefined ? {} : { place_uri: item.place_uri }),
        ...(item.geo_uri === undefined ? {} : { geo_uri: item.geo_uri }), ...(item.label === undefined ? {} : { label: item.label }),
        ...(item.timezone === undefined ? {} : { timezone: item.timezone }), ...(item.country_code === undefined ? {} : { country_code: item.country_code })
      })),
      identifiers: (profile.identifiers ?? []).map((item) => ({
        identifier_kind: item.identifier_kind as NonNullable<OrganizationProfileBody['identifiers']>[number]['identifier_kind'],
        identifier_value: item.identifier_value, ...envelopeBody(item.envelope)
      })),
      media: (profile.media ?? []).map((item) => ({
        media_kind: item.media_kind as NonNullable<OrganizationProfileBody['media']>[number]['media_kind'], original_value: item.original_value,
        ...envelopeBody(item.envelope),
        ...(item.content_hash === undefined ? {} : { content_hash: item.content_hash }), ...(item.uri === undefined ? {} : { uri: item.uri }),
        ...(item.media_type === undefined ? {} : { media_type: item.media_type })
      }))
    };
  }
</script>

<form onsubmit={(event) => { event.preventDefault(); void saveOrganization(); }}>
<Modal
  title={initialOrganization ? `Edit ${currentProfile?.organization.name ?? initialOrganization.name ?? 'organization'}` : 'Create organization'}
  ariaLabel={initialOrganization ? `Edit ${currentProfile?.organization.name ?? initialOrganization.name ?? 'organization'}` : 'Create organization'}
  closable={true}
  closeLabel="Close organization editor"
  onclose={requestClose}
  width="640px"
  maxWidth="min(640px, calc(100vw - 32px))"
>
  <div class="editor" aria-busy={loading || submitting}>
    {#if confirmDelete}
      <section class="deletion" aria-label="Delete organization confirmation">
        <h3>Delete {currentProfile?.organization.name}?</h3>
        {#if checkingDelete}<p role="status">Checking employment records…</p>
        {:else if deleteError}<p role="alert">{deleteError} <button type="button" class="retry" aria-label="Retry employment check" onclick={() => void checkDeletion()}>Retry</button></p>
        {:else if holders && holders.count > 0}
          <p>{currentProfile?.organization.name} has {holders.atLeast ? 'at least ' : ''}{holders.count} employment {holders.count === 1 ? 'record' : 'records'}. Remove {holders.count === 1 ? 'it' : 'them'} first.</p>
          <ul class="saved-rows">{#each holders.people as person, index}<li><span>{person}</span><span class="meta">{holders.descriptions[index]}</span></li>{/each}</ul>
          {#if holders.count > holders.people.length}<p class="meta">and {holders.count - holders.people.length} more</p>{/if}
        {:else}<p>This permanently removes the organization.</p>{/if}
      </section>
    {:else if loading}<p role="status">Loading current organization…</p>{:else}
        <label>Name<TextInput ariaLabel="Organization name" bind:value={name} required block disabled={submitting} /></label>
        <label>Kind<SelectDropdown title="Organization kind" value={kind} options={kindOptions} onchange={(value) => { kind = value; }} disabled={submitting} /></label>
        <label>Primary domain<TextInput ariaLabel="Organization primary domain" bind:value={primaryDomain} block disabled={submitting} /></label>
        <label>Aliases<TextInput ariaLabel="Organization aliases" bind:value={aliases} block disabled={submitting} /></label>
        <label>Categories<TextInput ariaLabel="Organization categories" bind:value={categories} block disabled={submitting} /></label>
        <label>Description<textarea aria-label="Organization description" bind:value={description} disabled={submitting}></textarea></label>
    {/if}
    {#if message}
      <div role="alert">
        <p>{message}{#if initialOrganization && !currentProfile && !loading}{' '}<button type="button" class="retry" aria-label="Retry organization load" onclick={() => void loadCurrent()}>Retry</button>{/if}</p>
        {#if conflictCurrent && !confirmDelete}
          <section class="current-data" aria-label="Current saved organization">
            <h3>Current saved organization</h3>
            <ul class="saved-rows">
              <li><span>{conflictCurrent.organization.name}</span><span class="meta">Name</span></li>
              <li><span>{sentenceCase(conflictCurrent.organization.kind)}</span><span class="meta">Kind</span></li>
              <li><span>{conflictCurrent.organization.primary_domain || 'None'}</span><span class="meta">Primary domain</span></li>
              <li><span>{conflictCurrent.organization.description || 'None'}</span><span class="meta">Description</span></li>
              <li><span>{conflictCurrent.organization.retired_at ? `Retired ${formatDateTime(conflictCurrent.organization.retired_at)}` : 'Active'}</span><span class="meta">Status</span></li>
            </ul>
            {#each [
              { title: 'Names', rows: (conflictCurrent.names ?? []).map(item => ({ value: item.name, envelope: item.envelope })) },
              { title: 'Categories', rows: (conflictCurrent.categories ?? []).map(item => ({ value: item.category, envelope: item.envelope })) },
              { title: 'Contact points', rows: (conflictCurrent.contact_points ?? []).map(item => ({ value: item.original_value, envelope: item.envelope })) },
              { title: 'Addresses', rows: (conflictCurrent.addresses ?? []).map(item => ({ value: item.original_value, envelope: item.envelope })) },
              { title: 'Identifiers', rows: (conflictCurrent.identifiers ?? []).map(item => ({ value: item.identifier_value, envelope: item.envelope })) },
              { title: 'Media', rows: (conflictCurrent.media ?? []).map(item => ({ value: present([item.original_value, item.media_type, item.uri]), envelope: item.envelope })) }
            ] as section}
              {#if section.rows.length}
                <h4>{section.title}</h4>
                <ul class="saved-rows">{#each section.rows as item}<li><span>{item.value}</span><span class="meta">{historyProvenance(item.envelope)}</span></li>{/each}</ul>
              {/if}
            {/each}
          </section>
        {/if}
      </div>
    {/if}
    {#if controller.createBlocked.organizations}<button type="button" class="retry" disabled={submitting} onclick={() => void refreshAfterUnknown()}>Refresh organizations</button>{/if}
  </div>
  {#snippet footer()}
    <div class="organization-actions">
    {#if confirmDelete}
      <Button label="Cancel" ariaLabel="Cancel organization deletion" disabled={submitting} onclick={() => { confirmDelete = false; message = ''; }} />
      <Button tone="danger" surface="solid" label="Delete" ariaLabel="Confirm delete organization" busy={submitting} disabled={checkingDelete || !!deleteError || !holders || holders.count > 0} onclick={() => void remove()} />
    {:else}
      {#if initialOrganization && currentProfile}<Button class="delete-organization" tone="danger" label="Delete organization" disabled={submitting} onclick={() => void checkDeletion()} />{/if}
      <Button label="Cancel" disabled={submitting} onclick={requestClose} />
      <Button tone={saveDisabled ? 'neutral' : 'info'} surface="solid" type="submit" label={initialOrganization ? 'Save organization' : 'Create organization'} busy={submitting} disabled={saveDisabled} />
    {/if}
    </div>
  {/snippet}
</Modal>
</form>

<style>
  .editor { display: grid; gap: var(--field-gap); }
  .deletion { display: grid; gap: var(--field-gap); }
  .organization-actions { display: flex; flex-wrap: wrap; align-items: center; justify-content: flex-end; gap: var(--field-gap); width: 100%; }
  :global(.kit-button.kit-button--danger.delete-organization) { margin-right: auto; padding-inline: 0; border-color: transparent; background: transparent; color: var(--text-danger); }
  label { display: grid; gap: var(--label-gap); color: var(--text-muted); font-size: var(--font-size-xs); }
  textarea { min-height: 5rem; resize: vertical; padding: var(--space-2); border: 1px solid var(--border-default); border-radius: var(--radius-sm); background: var(--bg-canvas); color: var(--text-primary); }
  h3, h4, p, ul { margin: 0; } [role="alert"] { color: var(--text-danger); }
  .current-data { display: grid; gap: var(--space-2); margin-top: var(--space-4); color: var(--text-primary); font-weight: normal; }
  .saved-rows { padding: 0; list-style: none; }
  .saved-rows li { display: flex; justify-content: space-between; gap: var(--field-gap); padding: var(--space-2) 0; border-bottom: 1px solid var(--border-muted); }
  .saved-rows li:last-child { border-bottom: 0; }
  .saved-rows span { min-width: 0; overflow-wrap: anywhere; }
  .meta { color: var(--text-muted); font-size: var(--font-size-sm); }
  .retry { justify-self: start; padding: 0; border: 0; background: none; color: var(--link-ink); font: inherit; cursor: pointer; }
  @media (max-width: 640px) {
    .saved-rows li { flex-direction: column; }
    .organization-actions { flex-direction: column-reverse; align-items: stretch; }
    .organization-actions :global(.kit-button) { width: 100%; }
    .organization-actions :global(.delete-organization) { margin-right: 0; }
  }
</style>
