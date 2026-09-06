<script lang="ts">
  import { appShortcuts, Button, Modal, SelectDropdown, TextInput } from '@kenn-io/kit-ui';
  import { onDestroy, onMount, untrack } from 'svelte';

  import type { DirectoryEntityController } from '../../directory/entity-controller.svelte';
  import type { DirectoryEntityMutationResult, Organization, OrganizationProfile, OrganizationProfileBody } from '../../directory/models';

  interface Props {
    controller: DirectoryEntityController;
    organization?: Organization;
    onDone?: () => void;
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
  let releaseScope: (() => void) | undefined;
  const kindOptions = ['company', 'nonprofit', 'school', 'government', 'household', 'other'].map((value) => ({ value, label: value[0]!.toUpperCase() + value.slice(1) }));

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
    if (submitting || !name.trim() || controller.createBlocked.organizations) return;
    submitting = true;
    message = '';
    conflictCurrent = undefined;
    try {
      const body = { name: name.trim(), kind: kind as 'company', primary_domain: primaryDomain.trim() || null, description: description.trim() || null };
      const result = initialOrganization
        ? await controller.updateOrganization(initialOrganization.id, body)
        : await controller.createOrganization(body);
      if (result.ok) { onDone(); return; }
      handleFailure(result);
    } finally {
      submitting = false;
    }
  }

  async function saveProfile(): Promise<void> {
    if (!initialOrganization || !currentProfile || submitting) return;
    submitting = true;
    message = '';
    conflictCurrent = undefined;
    try {
      const aliasDelta = valueDelta(baselineAliases, values(aliases));
      const categoryDelta = valueDelta(baselineCategories, values(categories));
      const result = await controller.putOrganizationProfile(
        initialOrganization.id,
        (freshProfile) => profileBody(freshProfile, aliasDelta, categoryDelta)
      );
      if (result.ok) { onDone(); return; }
      handleFailure(result);
    } finally {
      submitting = false;
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
    } finally {
      submitting = false;
      confirmDelete = false;
    }
  }

  function handleFailure(result: Exclude<DirectoryEntityMutationResult<unknown, Organization | OrganizationProfile>, { ok: true }>): void {
    if (result.kind === 'conflict') {
      conflictCurrent = asProfile(result.current);
      message = `This organization changed elsewhere. ${result.message}`;
    } else if (result.kind === 'unknown' || result.kind === 'blocked') {
      message = `The create outcome is unknown. ${result.message}`;
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

  function envelopeText(envelope: Name['envelope']): string {
    return present([
      envelope.source,
      envelope.source_ref,
      envelope.type_label,
      ...(envelope.type_tokens ?? []),
      envelope.pref === undefined ? undefined : `preference ${envelope.pref}`,
      envelope.confidence === undefined ? undefined : `confidence ${envelope.confidence}`,
      envelope.active_from,
      envelope.vcard.property,
      envelope.vcard.group,
      envelope.vcard.prop_id,
      ...(envelope.vcard.pid ?? []),
      envelope.vcard.altid
    ]);
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

<Modal
  title={initialOrganization ? `Edit ${initialOrganization.name}` : 'Create organization'}
  ariaLabel={initialOrganization ? `Edit ${initialOrganization.name}` : 'Create organization'}
  closeLabel="Close organization editor"
  onclose={requestClose}
  maxWidth="min(640px, calc(100vw - 32px))"
>
  <div class="editor" aria-busy={loading || submitting}>
    {#if loading}<p role="status">Loading current organization…</p>{:else}
      <form onsubmit={(event) => { event.preventDefault(); void saveOrganization(); }}>
        <label>Name<TextInput ariaLabel="Organization name" bind:value={name} required block disabled={submitting} /></label>
        <label>Kind<SelectDropdown title="Organization kind" value={kind} options={kindOptions} onchange={(value) => { kind = value; }} disabled={submitting} /></label>
        <label>Primary domain<TextInput ariaLabel="Organization primary domain" bind:value={primaryDomain} block disabled={submitting} /></label>
        <label>Description<textarea aria-label="Organization description" bind:value={description} disabled={submitting}></textarea></label>
        <Button type="submit" tone="info" surface="solid" label={initialOrganization ? 'Save organization' : 'Create organization'} disabled={submitting || !name.trim() || controller.createBlocked.organizations} />
      </form>
      {#if initialOrganization && currentProfile}
        <form onsubmit={(event) => { event.preventDefault(); void saveProfile(); }}>
          <h3>Structured profile</h3>
          <label>Aliases<TextInput ariaLabel="Organization aliases" bind:value={aliases} block disabled={submitting} /></label>
          <label>Categories<TextInput ariaLabel="Organization categories" bind:value={categories} block disabled={submitting} /></label>
          <p class="hint">Existing contact points, addresses, identifiers, and media references are preserved.</p>
          <Button type="submit" label="Save organization profile" disabled={submitting} />
        </form>
        {#if confirmDelete}
          <div role="group" aria-label="Delete organization confirmation"><p>Delete this organization if it has no employment records?</p><Button label="Cancel organization deletion" disabled={submitting} onclick={() => { confirmDelete = false; }} /><Button tone="danger" surface="solid" label="Confirm delete organization" disabled={submitting} onclick={() => void remove()} /></div>
        {:else}<Button tone="danger" label="Delete organization" disabled={submitting} onclick={() => { confirmDelete = true; }} />{/if}
      {/if}
    {/if}
    {#if message}
      <div role="alert">
        <p>{message}</p>
        {#if conflictCurrent}
          <section class="current-data" aria-label="Current saved organization">
            <h3>Current saved organization</h3>
            <dl>
              <div><dt>Name</dt><dd>{conflictCurrent.organization.name}</dd></div>
              <div><dt>Kind</dt><dd>{conflictCurrent.organization.kind}</dd></div>
              <div><dt>Primary domain</dt><dd>{conflictCurrent.organization.primary_domain || 'None'}</dd></div>
              <div><dt>Description</dt><dd>{conflictCurrent.organization.description || 'None'}</dd></div>
              <div><dt>Status</dt><dd>{conflictCurrent.organization.retired_at ? `Retired ${conflictCurrent.organization.retired_at}` : 'Active'}</dd></div>
            </dl>
            <h4>Names</h4>
            {#if conflictCurrent.names?.length}<ul>{#each conflictCurrent.names as item}<li>{item.name_kind}: {item.name} · {envelopeText(item.envelope)}</li>{/each}</ul>{:else}<p>None</p>{/if}
            <h4>Categories</h4>
            {#if conflictCurrent.categories?.length}<ul>{#each conflictCurrent.categories as item}<li>{item.category} · {envelopeText(item.envelope)}</li>{/each}</ul>{:else}<p>None</p>{/if}
            <h4>Contact points</h4>
            {#if conflictCurrent.contact_points?.length}<ul>{#each conflictCurrent.contact_points as item}<li>{present([item.address_kind, item.original_value, item.service_slug, item.scope_kind, item.scope_value, item.uri])} · {envelopeText(item.envelope)}</li>{/each}</ul>{:else}<p>None</p>{/if}
            <h4>Addresses</h4>
            {#if conflictCurrent.addresses?.length}<ul>{#each conflictCurrent.addresses as item}<li>{present([item.address_kind, item.original_value, item.post_office_box, item.extended_address, item.street_address, item.locality, item.region, item.postal_code, item.country_name, item.extended_components, item.free_text, item.place_uri, item.geo_uri, item.label, item.timezone, item.country_code])} · {envelopeText(item.envelope)}</li>{/each}</ul>{:else}<p>None</p>{/if}
            <h4>Identifiers</h4>
            {#if conflictCurrent.identifiers?.length}<ul>{#each conflictCurrent.identifiers as item}<li>{item.identifier_kind}: {item.identifier_value} · {envelopeText(item.envelope)}</li>{/each}</ul>{:else}<p>None</p>{/if}
            <h4>Media</h4>
            {#if conflictCurrent.media?.length}<ul>{#each conflictCurrent.media as item}<li>{present([item.media_kind, item.original_value, item.media_type, item.uri, item.content_hash])} · {envelopeText(item.envelope)}</li>{/each}</ul>{:else}<p>None</p>{/if}
          </section>
        {/if}
      </div>
    {/if}
    {#if controller.createBlocked.organizations}<Button label="Refresh organizations" disabled={submitting} onclick={() => void refreshAfterUnknown()} />{/if}
    <div class="actions"><Button label="Cancel" disabled={submitting} onclick={requestClose} /></div>
  </div>
</Modal>

<style>
  .editor, form { display: grid; gap: var(--space-3); }
  form + form { padding-top: var(--space-3); border-top: 1px solid var(--border-default); }
  label { display: grid; gap: var(--space-1); color: var(--text-muted); font-size: var(--font-size-xs); }
  textarea { min-height: 5rem; resize: vertical; padding: var(--space-2); border: 1px solid var(--border-default); border-radius: var(--radius-sm); background: var(--bg-canvas); color: var(--text-primary); }
  h3, h4, p, dl, dd, ul { margin: 0; } .hint { color: var(--text-muted); font-size: var(--font-size-xs); } [role="alert"] { color: var(--text-danger); }
  .current-data { display: grid; gap: var(--space-2); margin-top: var(--space-2); max-height: 20rem; overflow: auto; }
  .current-data dl { display: grid; gap: var(--space-1); } .current-data dl div { display: grid; grid-template-columns: 8rem 1fr; gap: var(--space-2); }
  .current-data dt, .current-data h4 { font-weight: 600; } .current-data ul { padding-left: var(--space-4); }
  .actions { display: flex; justify-content: flex-end; }
</style>
