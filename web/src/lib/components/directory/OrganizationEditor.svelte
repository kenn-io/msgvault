<script lang="ts">
  import { appShortcuts, Button, SelectDropdown, TextInput } from '@kenn-io/kit-ui';
  import { onDestroy, onMount, untrack } from 'svelte';

  import type { DirectoryEntityController } from '../../directory/entity-controller.svelte';
  import { organizationKindOptions as kindOptions } from '../../directory/labels';
  import type { Organization, OrganizationProfile, OrganizationProfileBody, Prepared } from '../../directory/models';

  import Dialog from '../common/Dialog.svelte';
  import InlineNotice from '../common/InlineNotice.svelte';
  import DetailRow from '../people/DetailRow.svelte';
  import { conflict, organizationFacts } from '../../directory/conflict';
  import { WriteAttempt } from '../../directory/write-attempt.svelte';
  import ConflictNotice from '../common/ConflictNotice.svelte';
  import { Disclosure } from '@kenn-io/kit-ui';
  import { valueProvenance, type ProvenanceLine } from './history-format';
  import { sentenceCase } from '../../explore/labels';

  interface Props {
    controller: DirectoryEntityController;
    organization?: Pick<Organization, 'id'> & Partial<Organization>;
    onDone?: (organization?: Organization) => void;
    onClose?: () => void;
    /** Opens a person who holds a job here, so a blocked delete can be resolved. */
    onOpenPerson?: (personID: number) => void;
    /** The holder's own job here, when it's on screen: the blocked delete opens it directly instead of the person. */
    holderJob?: (personID: number) => { label: string; open: () => void } | undefined;
  }

  let { controller, organization = undefined, onDone = () => undefined, onClose = () => undefined, onOpenPerson = undefined, holderJob = undefined }: Props = $props();
  const initialOrganization = untrack(() => organization);
  let name = $state(initialOrganization?.name ?? '');
  let kind = $state(initialOrganization?.kind ?? 'company');
  let primaryDomain = $state(initialOrganization?.primary_domain ?? '');
  let description = $state(initialOrganization?.description ?? '');
  let aliases = $state('');
  let categories = $state('');
  let baselineAliases = $state<string[]>([]);
  let baselineCategories = $state<string[]>([]);
  // Empty Aliases and Categories wait behind Add alias… and Add category… until the person asks for them.
  let addingAliases = $state(false);
  let addingCategories = $state(false);
  const aliasesShown = $derived(addingAliases || baselineAliases.length > 0);
  const categoriesShown = $derived(addingCategories || baselineCategories.length > 0);
  let contactDetailsOpen = $state(false);
  // The organization saved but its aliases and categories didn't.
  let profileUnsaved = $state(false);
  let currentProfile = $state<OrganizationProfile>();
  // The version a write sends as If-Match. Saving the details moves past it, so the next write reads afresh.
  let opened: Prepared<OrganizationProfile> | undefined;
  let deleteProfile = $state<Prepared<OrganizationProfile>>();
  let loading = $state(!!initialOrganization);
  // A failed load leaves nothing to edit, so the form and Save give way to the notice.
  const loadFailed = $derived(!!initialOrganization && !currentProfile && !loading);
  const attempt = new WriteAttempt<Prepared<OrganizationProfile>>();
  const removal = new WriteAttempt<Prepared<OrganizationProfile>>();
  const submitting = $derived(attempt.busy || removal.busy);
  let confirmDelete = $state(false);
  let checkingDelete = $state(false);
  let checkError = $state('');
  let holders = $state<Awaited<ReturnType<DirectoryEntityController['organizationEmploymentHolders']>>>();
  const deleteBlocked = $derived(checkingDelete || !!checkError || !holders || holders.count > 0);
  const saveDisabled = $derived(loading || (!!initialOrganization && !currentProfile) || !name.trim() || controller.createBlocked.organizations);
  let releaseScope: (() => void) | undefined;

  onMount(() => {
    releaseScope = appShortcuts.pushScope('directory-organization-editor');
    if (initialOrganization) void loadCurrent();
  });
  onDestroy(() => releaseScope?.());

  async function loadCurrent(): Promise<void> {
    if (!initialOrganization) return;
    loading = true;
    attempt.details = '';
    try {
      const { data: profile } = (opened = await controller.prepareOrganizationMutation(initialOrganization.id));
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
      attempt.details = cause instanceof Error ? cause.message : 'No response from the server.';
    } finally {
      loading = false;
    }
  }

  async function saveOrganization(): Promise<void> {
    if (saveDisabled || submitting) return;
    profileUnsaved = false;
    let saved = currentProfile?.organization;
    const ok = await attempt.run(async newer => {
      const body = { name: name.trim(), kind: kind as 'company', primary_domain: primaryDomain.trim() || null, description: description.trim() || null };
      let prepared = newer ?? opened;
      if (newer || !saved || Object.entries(body).some(([key, value]) => value !== (saved?.[key as keyof Organization] ?? null))) {
        const result = initialOrganization || saved
          ? await controller.updateOrganization((initialOrganization ?? saved)!.id, body, prepared)
          : await controller.createOrganization(body);
        if (!result.ok) return result;
        saved = result.entity!;
        currentProfile = { names: [], categories: [], contact_points: [], addresses: [], identifiers: [], media: [], ...currentProfile, organization: saved };
        prepared = opened = undefined;
      }
      if (newer || aliasDelta.added.length || aliasDelta.removed.size || categoryDelta.added.length || categoryDelta.removed.size) {
        const draft = draftLists();
        const result = await controller.putOrganizationProfile(saved!.id, fresh => ({
          ...profileBody(fresh),
          names: [...(fresh.names ?? []).filter(item => item.name_kind !== 'alias').map(nameBody), ...draft.names.filter(item => item.name_kind === 'alias')],
          categories: draft.categories,
        }), prepared);
        if (!result.ok) profileUnsaved = true;
        return result;
      }
      return { ok: true };
    });
    if (ok) onDone(saved);
  }

  function draftLists() {
    const profile = currentProfile!;
    return { names: namesWithDelta(profile, aliasDelta), categories: categoriesWithDelta(profile, categoryDelta) };
  }

  async function checkDeletion(): Promise<void> {
    if (!initialOrganization) return;
    if (!confirmDelete) { attempt.reset(); deleteProfile = opened; }
    confirmDelete = true;
    checkingDelete = true;
    checkError = '';
    holders = undefined;
    try {
      holders = await controller.organizationEmploymentHolders(initialOrganization.id);
    } catch (cause) {
      checkError = cause instanceof Error ? cause.message : 'No response from the server.';
    } finally {
      checkingDelete = false;
    }
  }

  async function remove(): Promise<void> {
    if (!initialOrganization || submitting || deleteBlocked) return;
    let collided = false;
    const ok = await removal.run(async () => {
      const result = await controller.deleteOrganization(initialOrganization.id, deleteProfile);
      collided = !result.ok && (result.kind === 'conflict' || (result.kind === 'error' && result.status === 409));
      return result;
    });
    if (ok) { onDone(); return; }
    if (collided) {
      if (removal.conflict) deleteProfile = removal.conflict;
      removal.reset();
      await checkDeletion();
    }
  }

  async function refreshAfterUnknown(): Promise<void> {
    await controller.refreshOrganizations();
    if (!controller.createBlocked.organizations) attempt.details = '';
  }

  function requestClose(): void {
    if (!submitting) { if (attempt.conflict) void controller.refreshOrganizations(); attempt.reset(); removal.reset(); onClose(); }
  }

  function values(value: string): string[] {
    return value.split(',').map((item) => item.trim()).filter(Boolean);
  }

  function present(parts: Array<string | number | undefined>): string {
    return parts.filter((part) => part !== undefined && part !== '').join(' · ');
  }

  // What a person auditing a saved value may need; the row itself shows the value and its provenance.
  const detailLabels: [string, string][] = [
    ['name_kind', 'Kind'], ['address_kind', 'Kind'], ['identifier_kind', 'Kind'], ['media_kind', 'Kind'],
    ['service_slug', 'Service'], ['scope_kind', 'Scope'], ['scope_value', 'Scope value'], ['uri', 'Link'],
    ['post_office_box', 'PO box'], ['extended_address', 'Extended address'], ['street_address', 'Street'],
    ['locality', 'City'], ['region', 'Region'], ['postal_code', 'Postal code'], ['country_name', 'Country'],
    ['country_code', 'Country code'], ['extended_components', 'Other parts'], ['free_text', 'Free text'],
    ['place_uri', 'Place link'], ['geo_uri', 'Coordinates'], ['label', 'Label'], ['timezone', 'Time zone']
  ];

  // Words first; the vCard properties a person might quote to support go in a last Raw values fold.
  function valueDetails(record: object, envelope: Name['envelope']): { facts: Fact[]; raw: Fact[] } {
    const fields = record as Record<string, unknown>;
    const text = (value: unknown) => Array.isArray(value) ? value.join(', ') : value === undefined || value === null || value === '' ? '' : typeof value === 'object' ? JSON.stringify(value) : String(value);
    const filled = (facts: Fact[]) => facts.filter((fact) => fact.value);
    return {
      facts: filled([
        ...detailLabels.map(([key, label]) => ({ label, value: key.endsWith('_kind') ? sentenceCase(text(fields[key])) : text(fields[key]) })),
        { label: 'Type', value: text(envelope.type_label) },
        { label: 'Preference', value: text(envelope.pref) },
        { label: 'Source reference', value: text(envelope.source_ref) },
        { label: 'Confidence', value: envelope.confidence === undefined ? '' : `${Math.round(envelope.confidence * 100)}%` }
      ]),
      raw: filled([
        { label: 'Type tokens', value: text(envelope.type_tokens) },
        { label: 'vCard property', value: text(envelope.vcard.property) },
        { label: 'vCard group', value: text(envelope.vcard.group) },
        { label: 'vCard property ID', value: text(envelope.vcard.prop_id) },
        { label: 'vCard PID', value: text(envelope.vcard.pid) },
        { label: 'vCard ALTID', value: text(envelope.vcard.altid) }
      ])
    };
  }

  type Fact = { label: string; value: string };
  type ProfileRow = { value: string; envelope: Name['envelope']; record: object };
  const CONTACT_LABELS: Record<string, string> = { email: 'Email', phone: 'Phone', url: 'Website' };

  // The structured profile the form doesn't edit, grouped as a person names it: Email, Phone, Address.
  function profileGroups(profile: OrganizationProfile, sections: ('names' | 'categories' | 'contact_points' | 'addresses' | 'identifiers' | 'media')[]): { title: string; rows: ProfileRow[] }[] {
    const groups = new Map<string, ProfileRow[]>();
    const add = (title: string, row: ProfileRow) => groups.set(title, [...(groups.get(title) ?? []), row]);
    if (sections.includes('names')) for (const item of profile.names ?? []) add('Name', { value: item.name, envelope: item.envelope, record: item });
    if (sections.includes('categories')) for (const item of profile.categories ?? []) add('Category', { value: item.category, envelope: item.envelope, record: item });
    if (sections.includes('contact_points')) for (const item of profile.contact_points ?? []) add(CONTACT_LABELS[item.address_kind] ?? sentenceCase(item.address_kind), { value: item.original_value, envelope: item.envelope, record: item });
    if (sections.includes('addresses')) for (const item of profile.addresses ?? []) add('Address', { value: item.original_value, envelope: item.envelope, record: item });
    if (sections.includes('identifiers')) for (const item of profile.identifiers ?? []) add('Identifier', { value: item.identifier_value, envelope: item.envelope, record: item });
    if (sections.includes('media')) for (const item of profile.media ?? []) add('Media', { value: present([item.original_value, item.media_type, item.uri]), envelope: item.envelope, record: item });
    return [...groups].map(([title, rows]) => ({ title, rows }));
  }

  // The provenance most rows share, said once above the groups when two or more share it; a row keeps its own line only when it differs.
  function sharedProvenance(groups: { rows: ProfileRow[] }[]): ProvenanceLine | undefined {
    const counts = new Map<string, { line: ProvenanceLine; count: number }>();
    for (const row of groups.flatMap((group) => group.rows)) {
      const line = valueProvenance(row.envelope);
      const seen = counts.get(line.text);
      counts.set(line.text, { line: seen?.line ?? line, count: (seen?.count ?? 0) + 1 });
    }
    const top = [...counts.values()].sort((a, b) => b.count - a.count)[0];
    return top && top.count > 1 ? top.line : undefined;
  }

  // Structured sections that changed since the person opened the organization.
  const STRUCTURED = ['names', 'categories', 'contact_points', 'addresses', 'identifiers', 'media'] as const;

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

  const aliasDelta = $derived(valueDelta(baselineAliases, aliases === baselineAliases.join(', ') ? baselineAliases : values(aliases)));
  const categoryDelta = $derived(valueDelta(baselineCategories, categories === baselineCategories.join(', ') ? baselineCategories : values(categories)));

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

  function profileBody(profile: OrganizationProfile): OrganizationProfileBody {
    return {
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

{#snippet profileGroupsBlock(groups: { title: string; rows: ProfileRow[] }[], heading: string | undefined)}
  {@const shared = sharedProvenance(groups)}
  {@const rows = groups.flatMap((group) => group.rows.map((row) => ({ title: group.title, row, details: valueDetails(row.record, row.envelope) })))}
  {@const raw = rows.filter((item) => item.details.raw.length)}
  <div class="profile">
    <!-- One heading over the rows: "Contact details · From CardDAV contacts · Nov 6", and one Details for all of them. -->
    {#if heading || shared}<div class="profile-heading">
      <p>{#if heading}<span class="heading">{heading}</span>{/if}{#if heading && shared}{' · '}{/if}{#if shared}<span class="provenance" title={shared.title}>{shared.text}</span>{/if}</p>
      {#if rows.some((item) => item.details.facts.length || item.details.raw.length)}<Button label="Details" class="text-action" size="sm" ariaExpanded={contactDetailsOpen} onclick={() => contactDetailsOpen = !contactDetailsOpen} />{/if}
    </div>{/if}
    {#if contactDetailsOpen || !heading}
      <div class="profile-details">
        {#each rows as item}{#if item.details.facts.length}
          <section aria-label={`${item.title} ${item.row.value}`}><h5>{item.title} · {item.row.value}</h5><dl class="facts">{#each item.details.facts as fact}<div><dt>{fact.label}</dt><dd>{fact.value}</dd></div>{/each}</dl></section>
        {/if}{/each}
        {#if raw.length}<Disclosure variant="plain" class="fold" title="Raw values">{#each raw as item}<h5>{item.title} · {item.row.value}</h5><dl class="facts">{#each item.details.raw as fact}<div><dt>{fact.label}</dt><dd>{fact.value}</dd></div>{/each}</dl>{/each}</Disclosure>{/if}
      </div>
    {/if}
    {#each groups as group (group.title)}
      <!-- Email, Phone, and Address as label | value rows; a second value of one kind keeps the label column empty. -->
      <section class="profile-group" aria-label={group.title}>{#each group.rows as item, index}{@const line = valueProvenance(item.envelope)}<DetailRow label={index === 0 ? group.title : ''} description={line.text === shared?.text ? undefined : line}>{item.value}</DetailRow>{/each}</section>
    {/each}
  </div>
{/snippet}
{#snippet deleteStart()}{#if initialOrganization && currentProfile && !confirmDelete}<Button tone="danger" label="Delete organization…" disabled={submitting} onclick={() => void checkDeletion()} />{/if}{/snippet}
{#if confirmDelete}
  <Dialog title={`Delete ${name}?`} onclose={requestClose} onback={() => { confirmDelete = false; attempt.reset(); removal.reset(); }} backLabel={`Back to ${name}`}
    primaryLabel={deleteBlocked ? undefined : 'Delete'} primaryTone="danger" cancellable={false} busy={submitting} onprimary={() => void remove()}>
    <div class="delete-step">
      {#if checkingDelete}<p role="status">Checking jobs…</p>{/if}
      {#if checkError}<InlineNotice message="Couldn't check jobs." details={checkError} actionLabel="Check again" onaction={() => void checkDeletion()} />{/if}
      {#if holders && holders.count > 0}
        <p>{holders.count === 1 && !holders.atLeast ? "Delete its job first. Ending it isn't enough." : `Delete its ${holders.count}${holders.atLeast ? ' or more' : ''} jobs first. Ending them isn't enough.`}</p>
        <!-- The open person's own job is deleted from here. Anyone else's is deleted on their page, so their row's
             action opens it and says why. -->
        <div class="holders">{#each holders.holders as holder}{@const job = holderJob?.(holder.personID)}
          <DetailRow description={holder.job} actionLabel={holder.name}
            menu={job ? [{ label: job.label, onselect: () => { job.open(); onClose(); } }] : onOpenPerson ? [{ label: 'Open to delete…', ariaLabel: `Open to delete ${holder.name}’s job`, onselect: () => { onClose(); onOpenPerson(holder.personID); } }] : []}>
            {#if onOpenPerson}<button type="button" data-link onclick={() => { onClose(); onOpenPerson(holder.personID); }}>{holder.name}</button>{:else}{holder.name}{/if}
          </DetailRow>{/each}</div>
        {#if holders.peopleCount > holders.holders.length}<p>and {holders.atLeast ? 'at least ' : ''}{holders.peopleCount - holders.holders.length} more</p>{/if}
      {/if}
      {#if !deleteBlocked}<p>This permanently removes the organization.</p>{/if}
    </div>
    {#snippet notice()}{#if removal.failed}<InlineNotice message="Couldn't delete this organization." details={removal.details} actionLabel="Retry" onaction={() => void remove()} />{/if}{/snippet}
  </Dialog>
{:else}
<Dialog title={currentProfile?.organization.name ?? initialOrganization?.name ?? (initialOrganization ? 'this organization' : 'Add organization')} onclose={requestClose} footerStart={loadFailed ? undefined : deleteStart} primaryLabel={loadFailed ? undefined : attempt.conflict ? 'Keep yours' : initialOrganization ? 'Save' : 'Add organization'} primaryDisabled={saveDisabled} busy={submitting} onprimary={() => void saveOrganization()}
  cancelLabel={attempt.conflict ? 'Use saved' : 'Cancel'}>
{#if !loadFailed}<form onsubmit={(event) => { event.preventDefault(); void saveOrganization(); }}>
  <div class="editor" aria-busy={loading || submitting}>
    {#if loading}<p role="status">Loading organization…</p>{:else}
        <label>Name<TextInput ariaLabel="Organization name" bind:value={name} required block size="lg" disabled={submitting} /></label>
        <div class="form-pair">
          <label>Kind<SelectDropdown title="Organization kind" value={kind} options={kindOptions} onchange={(value) => { kind = value; }} disabled={submitting} /></label>
          <label>Domain<TextInput ariaLabel="Organization domain" bind:value={primaryDomain} block size="lg" disabled={submitting} /></label>
        </div>
        {#if currentProfile}
          {@const groups = profileGroups(currentProfile, ['contact_points', 'addresses'])}
          {#if groups.length}{@render profileGroupsBlock(groups, 'Contact details')}{/if}
        {/if}
        <label>Description<textarea aria-label="Organization description" bind:value={description} disabled={submitting}></textarea></label>
        {#if aliasesShown || categoriesShown}<div class="form-pair">
          {#if aliasesShown}<label>Aliases<TextInput ariaLabel="Organization aliases" bind:value={aliases} block size="lg" disabled={submitting} /></label>{/if}
          {#if categoriesShown}<label>Categories<TextInput ariaLabel="Organization categories" bind:value={categories} block size="lg" disabled={submitting} /></label>{/if}
        </div>{/if}
        {#if !aliasesShown || !categoriesShown}<div class="add-links">
          {#if !aliasesShown}<button type="button" class="add-link kit-control-states" disabled={submitting} onclick={() => addingAliases = true}>Add alias…</button>{/if}
          {#if !categoriesShown}<button type="button" class="add-link kit-control-states" disabled={submitting} onclick={() => addingCategories = true}>Add category…</button>{/if}
        </div>{/if}
    {/if}
  </div>
<button type="submit" hidden>Save</button>
</form>{/if}
  {#snippet notice()}
    {#if attempt.conflict}
      {@const savedFacts = organizationFacts(attempt.conflict.data)}
      {@const openedFacts = organizationFacts(currentProfile!)}
      {@const draft = draftLists()}
      {@const yours = { Name: name.trim(), Kind: sentenceCase(kind), Domain: primaryDomain.trim(), Description: description.trim(), Aliases: draft.names.filter(item => item.name_kind === 'alias').map(item => item.name).join(', '), Categories: draft.categories.map(item => item.category).join(', ') }}
      <ConflictNotice noun="organization" saved={attempt.conflict.data.organization} {...conflict(savedFacts, openedFacts, yours)} message={attempt.details}>
        {#snippet more()}
          {@const groups = profileGroups(attempt.conflict!.data, [...STRUCTURED])}
          {#if groups.length}{@render profileGroupsBlock(groups, undefined)}{/if}
        {/snippet}
      </ConflictNotice>
    {:else if attempt.details || controller.createBlocked.organizations}
      <InlineNotice message={controller.createBlocked.organizations ? "Couldn't confirm the organization was saved." : !currentProfile && initialOrganization ? "Couldn't load this organization." : profileUnsaved ? "Couldn't save the aliases and categories." : "Couldn't save this organization."} details={attempt.details || undefined} actionLabel={controller.createBlocked.organizations ? 'Check again' : 'Retry'} onaction={() => { if (controller.createBlocked.organizations) void refreshAfterUnknown(); else if (initialOrganization && !currentProfile) void loadCurrent(); else void saveOrganization(); }} />
    {/if}
  {/snippet}
</Dialog>
{/if}

<style>
  /* L4: fields 16 apart, each label (Meta, 600) 4 above its field; pairs are .form-pair. */
  .editor { display: grid; gap: var(--space-pane); min-width: 0; }
  label { display: grid; gap: var(--space-tight); align-content: start; min-width: 0; color: var(--text-secondary); font-size: var(--type-meta-size); font-weight: var(--font-weight-semibold); line-height: var(--type-meta-line); }
  label > :global(*) { font-weight: normal; }
  textarea { resize: vertical; border: 1px solid var(--border-default); border-radius: var(--radius-sm); background: var(--bg-canvas); color: var(--text-primary); font: inherit; font-weight: normal; }
  p { margin: 0; }
  /* The contact block is its own group, 24 from the fields around it: its heading row, then the rows 8 below. */
  .profile { display: grid; gap: var(--space-group); min-width: 0; }
  .editor > .profile { margin-block: var(--space-group); }
  .profile-group { display: grid; }
  /* The heading row is 28, so Details (a 28 text action) sits on it; Heading text, provenance in Meta. When the
   * heading wraps on a phone, Details stays on its first line. */
  .profile-heading { display: flex; align-items: baseline; justify-content: space-between; gap: var(--space-box); min-height: var(--control-h); }
  .profile-heading p { min-width: 0; font-size: var(--type-meta-size); line-height: var(--type-meta-line); }
  .heading { color: var(--text-secondary); font-size: var(--type-heading-size); font-weight: var(--font-weight-semibold); line-height: var(--type-heading-line); }
  .provenance { color: var(--text-muted); }
  /* The delete step: its sentence, the people holding jobs, and what's left, 16 apart. */
  .delete-step { display: grid; gap: var(--space-pane); min-width: 0; font-size: var(--type-body-size); line-height: var(--type-body-line); }
  .holders { display: grid; }
  .profile-details { display: grid; gap: var(--space-group); }
  .profile-details section { display: grid; }
  h5 { margin: 0; color: var(--text-secondary); font-size: var(--type-meta-size); font-weight: var(--font-weight-semibold); line-height: var(--type-meta-line); }
  /* Add alias… and Add category… are text actions on a 28 line, 12 apart. */
  .add-links { display: flex; flex-wrap: wrap; gap: var(--space-box); }
  .add-link { display: inline-flex; align-items: center; min-height: var(--control-h); padding: 0; border: 0; background: none; color: var(--link-ink); font: inherit; font-size: var(--type-meta-size); line-height: var(--type-meta-line); cursor: pointer; }
  /* A value's facts are L3 label | value rows on the 160 column. */
  .facts { display: grid; margin: 0; }
  .facts div { display: grid; grid-template-columns: var(--detail-label-width) minmax(0, 1fr); align-items: baseline; gap: var(--space-group); padding-block: var(--row-pad-y); }
  .facts dt { color: var(--text-muted); font-size: var(--type-meta-size); line-height: var(--type-meta-line); }
  .facts dd { margin: 0; overflow-wrap: anywhere; font-size: var(--type-body-size); line-height: var(--type-body-line); }
</style>
