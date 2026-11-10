<script lang="ts">
  import { Button, SelectDropdown, TextInput } from '@kenn-io/kit-ui';
  import { onMount, untrack } from 'svelte';

  import type { DirectoryEntityController } from '../../directory/entity-controller.svelte';
  import { organizationKindOptions as kindOptions } from '../../directory/labels';
  import { CONTACT_LABELS } from '../../directory/contact-labels';
  import type { Organization, OrganizationProfile, OrganizationProfileBody, Prepared } from '../../directory/models';

  import Dialog from '../common/Dialog.svelte';
  import InlineNotice from '../common/InlineNotice.svelte';
  import DetailRow from '../people/DetailRow.svelte';
  import { organizationFacts } from '../../directory/conflict';
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
  }

  let { controller, organization = undefined, onDone = () => undefined, onClose = () => undefined, onOpenPerson = undefined }: Props = $props();
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
  // The latest data known to the dialog and its version.
  let opened = $state<Prepared<OrganizationProfile>>();
  // A pending draft keeps its revision when the other section resolves a conflict.
  let heldOrganizationEtag = $state<string>();
  let heldProfileEtag = $state<string>();
  const currentProfile = $derived(opened?.data);
  const load = new WriteAttempt<Prepared<OrganizationProfile>>();
  const attempt = new WriteAttempt<Prepared<OrganizationProfile>>();
  const loading = $derived(!!initialOrganization && !currentProfile && !load.details);
  // A failed load leaves nothing to edit, so the form and Save give way to the notice.
  const loadFailed = $derived(!!initialOrganization && !currentProfile && !!load.details);
  // Aliases and categories save on their own, with their own If-Match.
  const profileAttempt = new WriteAttempt<Prepared<OrganizationProfile>>();
  const removal = new WriteAttempt<Prepared<OrganizationProfile>>();
  const submitting = $derived(attempt.busy || profileAttempt.busy || removal.busy);
  let confirmDelete = $state(false);
  // The jobs check: reset on Back, close, and each new check, so a late result from an earlier one is dropped.
  const check = new WriteAttempt<Awaited<ReturnType<DirectoryEntityController['organizationEmploymentHolders']>>>();
  const holders = $derived(check.prepared);
  const deleteBlocked = $derived(!holders || holders.count > 0);
  const saveDisabled = $derived(loading || (!!initialOrganization && !currentProfile) || !name.trim() || controller.createBlocked.organizations);
  const profileDirty = $derived(aliases !== baselineAliases.join(', ') || categories !== baselineCategories.join(', '));
  const organizationDirty = $derived(!!currentProfile && (name !== currentProfile.organization.name || kind !== currentProfile.organization.kind || primaryDomain !== (currentProfile.organization.primary_domain ?? '') || description !== (currentProfile.organization.description ?? '')));

  onMount(() => {
    if (initialOrganization) void loadCurrent();
  });

  async function loadCurrent(): Promise<void> {
    if (!initialOrganization) return;
    const loaded = await load.prepare(() => controller.prepareOrganizationMutation(initialOrganization.id));
    if (!loaded) return;
    opened = loaded;
    showOrganization(loaded.data.organization);
    showLists(loaded.data);
  }

  function showOrganization(organization: Organization): void {
    name = organization.name;
    kind = organization.kind;
    primaryDomain = organization.primary_domain ?? '';
    description = organization.description ?? '';
  }

  function showLists(profile: OrganizationProfile): void {
    baselineAliases = (profile.names ?? []).filter((item) => item.name_kind === 'alias').map((item) => item.name);
    baselineCategories = (profile.categories ?? []).map((item) => item.category);
    aliases = baselineAliases.join(', ');
    categories = baselineCategories.join(', ');
  }

  async function saveOrganization(): Promise<void> {
    if (saveDisabled || submitting) return;
    const resolved = !!attempt.prepared && attempt.prepared.etag !== opened?.etag;
    // The notice and Retry follow the latest save.
    profileAttempt.reset();
    let entity: Organization | undefined;
    let saved: Prepared<OrganizationProfile> | undefined;
    const ok = await attempt.run(async current => {
      const body = { name: name.trim(), kind: kind as 'company', primary_domain: primaryDomain.trim() || null, description: description.trim() || null };
      const sent = current ?? (opened && { ...opened, etag: heldOrganizationEtag ?? opened.etag });
      const result = initialOrganization
        ? await controller.updateOrganization(initialOrganization.id, body, sent)
        : await controller.createOrganization(body);
      if (result.ok) entity = result.entity;
      if (result.ok && result.entity && result.etag && sent) saved = { data: { ...sent.data, organization: result.entity }, etag: result.etag };
      return result;
    });
    if (!ok) return;
    heldOrganizationEtag = undefined;
    if (saved && profileDirty) { if (resolved) heldProfileEtag ??= opened?.etag; opened = saved; showOrganization(saved.data.organization); }
    else onDone(entity);
  }

  async function saveProfile(): Promise<void> {
    if (!initialOrganization || !currentProfile || submitting) return;
    const resolved = !!profileAttempt.prepared && profileAttempt.prepared.etag !== opened?.etag;
    attempt.reset();
    let saved: Prepared<OrganizationProfile> | undefined;
    const ok = await profileAttempt.run(async current => {
      const result = await controller.putOrganizationProfile(initialOrganization.id, listsBody, current ?? (opened && { ...opened, etag: heldProfileEtag ?? opened.etag }));
      if (result.ok && result.entity && result.etag) saved = { data: result.entity, etag: result.etag };
      return result;
    });
    if (!ok) return;
    heldProfileEtag = undefined;
    if (saved && organizationDirty) { if (resolved) heldOrganizationEtag ??= opened?.etag; opened = saved; showLists(saved.data); }
    else onDone();
  }

  // Aliases and categories as the form shows them, over the rest of the version sent.
  function listsBody(profile: OrganizationProfile): OrganizationProfileBody {
    const saved = (profile.names ?? []).filter((item) => item.name_kind === 'alias').map((item) => item.name);
    return profileBody(profile, valueDelta(saved, values(aliases)), valueDelta((profile.categories ?? []).map((item) => item.category), values(categories)));
  }

  async function checkDeletion(): Promise<void> {
    if (!initialOrganization) return;
    confirmDelete = true;
    check.reset();
    await check.prepare(() => controller.organizationEmploymentHolders(initialOrganization.id));
  }

  async function remove(): Promise<void> {
    if (!initialOrganization || submitting || deleteBlocked) return;
    if (removal.reloadNeeded) await removal.reload(() => controller.prepareOrganizationMutation(initialOrganization.id));
    else if (await removal.run(current => controller.deleteOrganization(initialOrganization.id, current))) onDone(); else await check.prepare(() => controller.organizationEmploymentHolders(initialOrganization.id));
  }

  async function refreshAfterUnknown(): Promise<void> {
    await controller.refreshOrganizations();
    if (!controller.createBlocked.organizations) attempt.details = '';
  }

  function requestClose(): void {
    if (!submitting) { load.reset(); attempt.reset(); profileAttempt.reset(); removal.reset(); check.reset(); onClose(); }
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

  // The structured profile the form doesn't edit, grouped as a person names it: Email, Phone, Address. A contact's
  // kind is its group's title, so its facts leave it out.
  function profileGroups(profile: OrganizationProfile, sections: ('names' | 'categories' | 'contact_points' | 'addresses' | 'identifiers' | 'media')[]): { title: string; rows: ProfileRow[] }[] {
    const groups = new Map<string, ProfileRow[]>();
    const add = (title: string, row: ProfileRow) => groups.set(title, [...(groups.get(title) ?? []), row]);
    if (sections.includes('names')) for (const item of profile.names ?? []) add('Name', { value: item.name, envelope: item.envelope, record: item });
    if (sections.includes('categories')) for (const item of profile.categories ?? []) add('Category', { value: item.category, envelope: item.envelope, record: item });
    if (sections.includes('contact_points')) for (const item of profile.contact_points ?? []) add(CONTACT_LABELS[item.address_kind] ?? sentenceCase(item.address_kind), { value: item.original_value, envelope: item.envelope, record: { ...item, address_kind: undefined } });
    if (sections.includes('addresses')) for (const item of profile.addresses ?? []) add('Address', { value: item.original_value, envelope: item.envelope, record: item });
    if (sections.includes('identifiers')) for (const item of profile.identifiers ?? []) add('Identifier', { value: item.identifier_value, envelope: item.envelope, record: item });
    if (sections.includes('media')) for (const item of profile.media ?? []) add('Media', { value: present([item.original_value, item.media_type, item.uri === item.original_value ? undefined : item.uri]), envelope: item.envelope, record: { ...item, uri: undefined } });
    return [...groups].map(([title, rows]) => ({ title, rows }));
  }

  // The provenance most rows share, said once above the groups when two or more share it; a row keeps its own line only when it differs.
  function sharedProvenance(groups: { rows: ProfileRow[] }[]): ProvenanceLine | undefined {
    const counts = new Map<string, { line: ProvenanceLine; count: number }>();
    for (const row of groups.flatMap((group) => group.rows)) {
      const line = valueProvenance(row.envelope);
      const key = JSON.stringify([line.text, line.title]);
      const seen = counts.get(key);
      counts.set(key, { line: seen?.line ?? line, count: (seen?.count ?? 0) + 1 });
    }
    const top = [...counts.values()].sort((a, b) => b.count - a.count)[0];
    return top && top.count > 1 ? top.line : undefined;
  }

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
    const retained = (profile.names ?? []).filter((item) => item.name_kind !== 'alias' || !delta.removed.has(item.name));
    const nextOrdinal = Math.max(-1, ...(profile.names ?? []).map((item) => item.envelope.ordinal)) + 1;
    return [...retained.map(nameBody), ...delta.added.map((name, index): NameBody => ({ name, name_kind: 'alias', ordinal: nextOrdinal + index, source: 'user' }))];
  }

  function nameBody(item: Name): NameBody {
    return { name: item.name, name_kind: item.name_kind as NameBody['name_kind'], ...envelopeBody(item.envelope) };
  }

  function categoriesWithDelta(profile: OrganizationProfile, delta: ValueDelta): CategoryBody[] {
    const retained = (profile.categories ?? []).filter((item) => !delta.removed.has(item.category));
    const nextOrdinal = Math.max(-1, ...(profile.categories ?? []).map((item) => item.envelope.ordinal)) + 1;
    return [...retained.map(categoryBody), ...delta.added.map((category, index): CategoryBody => ({ category, ordinal: nextOrdinal + index, source: 'user' }))];
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

{#snippet profileGroupsBlock(groups: { title: string; rows: ProfileRow[] }[], heading: string | undefined)}
  {@const shared = sharedProvenance(groups)}
  {@const rows = groups.flatMap((group) => group.rows.map((row) => ({ title: group.title, row, details: valueDetails(row.record, row.envelope) })))}
  {@const raw = rows.filter((item) => item.details.raw.length)}
  {@const open = contactDetailsOpen || !heading}
  <div class="profile">
    <!-- One heading over the rows: "Contact details · From CardDAV contacts · Nov 6", and one Details for all of them. -->
    {#if heading || shared}<div class="profile-heading">
      <p>{#if heading}<span class="heading">{heading}</span>{/if}{#if heading && shared}{' · '}{/if}{#if shared}<span class="provenance" title={shared.title}>{shared.text}</span>{/if}</p>
      {#if heading && rows.some((item) => item.details.facts.length || item.details.raw.length)}<Button label="Details" class="text-action" size="sm" ariaExpanded={contactDetailsOpen} onclick={() => contactDetailsOpen = !contactDetailsOpen} />{/if}
    </div>{/if}
    {#each groups as group (group.title)}
      <!-- Email, Phone, and Address as label | value rows; a second value of one kind keeps the label column empty.
           With Details open, each value's facts sit under its own row, so a value shows once. -->
      <section class="profile-group" aria-label={group.title}>{#each group.rows as item, index}{@const line = valueProvenance(item.envelope)}{@const facts = valueDetails(item.record, item.envelope).facts}
        <DetailRow label={index === 0 ? group.title : ''} description={line.text === shared?.text && line.title === shared?.title ? undefined : line}>{item.value}</DetailRow>
        {#if open && facts.length}<section aria-label={`${group.title} ${item.value}`}><dl class="facts">{#each facts as fact}<div><dt>{fact.label}</dt><dd>{fact.value}</dd></div>{/each}</dl></section>{/if}
      {/each}</section>
    {/each}
    {#if open && raw.length}<Disclosure variant="plain" class="fold" title="Raw values">{#each raw as item}<h5>{item.title} · {item.row.value}</h5><dl class="facts">{#each item.details.raw as fact}<div><dt>{fact.label}</dt><dd>{fact.value}</dd></div>{/each}</dl>{/each}</Disclosure>{/if}
  </div>
{/snippet}
{#snippet conflictNotice(write: WriteAttempt<Prepared<OrganizationProfile>>, onaction: (() => void) | undefined, actionLabel = 'Keep yours')}
  {@const saved = write.conflict!.data}
  {@const groups = profileGroups(saved, [...STRUCTURED])}
  <ConflictNotice noun="organization" saved={saved.organization} record={organizationFacts(saved.organization)} message={write.details} actionLabel={onaction && actionLabel} {onaction}>
    {#if groups.length}{@render profileGroupsBlock(groups, undefined)}{/if}
  </ConflictNotice>
{/snippet}
{#snippet deleteStart()}{#if initialOrganization && currentProfile && !confirmDelete}<Button tone="danger" label="Delete organization…" disabled={submitting} onclick={() => void checkDeletion()} />{/if}{/snippet}
{#if confirmDelete}
  <Dialog title={`Delete ${name}?`} onclose={requestClose} onback={() => { confirmDelete = false; check.reset(); }} backLabel={`Back to ${name}`}
    primaryLabel={deleteBlocked ? undefined : 'Delete'} primaryTone="danger" cancellable={false} busy={submitting} onprimary={() => void remove()}>
    <div class="delete-step">
      {#if !holders && !check.details}<p role="status">Checking jobs…</p>{/if}
      {#if check.details}<InlineNotice message="Couldn't check jobs." details={check.details} actionLabel="Check again" onaction={() => void checkDeletion()} />{/if}
      {#if holders && holders.count > 0}
        <p>{holders.count === 1 && !holders.atLeast ? "Delete its job first. Ending it isn't enough." : `Delete its ${holders.count}${holders.atLeast ? ' or more' : ''} jobs first. Ending them isn't enough.`}</p>
        <div class="holders">{#each holders.holders as holder}
          <DetailRow description={holder.job}>
            {#if onOpenPerson}<button type="button" data-link onclick={() => { onClose(); onOpenPerson(holder.personID); }}>{holder.name}</button>{:else}{holder.name}{/if}
          </DetailRow>{/each}</div>
        {@const shownPeople = new Set(holders.holders.map(holder => holder.personID)).size}
        {#if holders.peopleCount > shownPeople}<p>and {holders.atLeast ? 'at least ' : ''}{holders.peopleCount - shownPeople} more</p>{:else if holders.atLeast}<p>and maybe others</p>{/if}
      {/if}
      {#if !deleteBlocked}<p>This permanently removes the organization.</p>{/if}
    </div>
    {#snippet notice()}{#if removal.conflict}{@render conflictNotice(removal, () => void remove(), 'Retry')}{:else if removal.failed && !deleteBlocked}<InlineNotice message="Couldn't delete this organization." details={removal.details} actionLabel="Retry" onaction={() => void remove()} />{/if}{/snippet}
  </Dialog>
{:else}
<Dialog title={currentProfile?.organization.name ?? initialOrganization?.name ?? (initialOrganization ? 'this organization' : 'Add organization')} onclose={requestClose} footerStart={loadFailed ? undefined : deleteStart} primaryLabel={loadFailed ? undefined : attempt.conflict ? 'Keep yours' : initialOrganization ? 'Save organization' : 'Add organization'} primaryDisabled={saveDisabled} busy={submitting} onprimary={() => void saveOrganization()}
  cancelLabel={attempt.conflict || profileAttempt.conflict ? 'Use saved' : 'Cancel'}>
{#if !loadFailed}
  <div class="editor" aria-busy={loading || submitting}>
    {#if loading}<p role="status">Loading organization…</p>{:else}
      <form onsubmit={(event) => { event.preventDefault(); void saveOrganization(); }}>
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
        <button type="submit" hidden>Save organization</button>
      </form>
      <!-- A new organization gets aliases and categories once it exists. -->
      {#if currentProfile}<form onsubmit={(event) => { event.preventDefault(); void saveProfile(); }}>
        {#if aliasesShown || categoriesShown}<div class="form-pair">
          {#if aliasesShown}<label>Aliases<TextInput ariaLabel="Organization aliases" bind:value={aliases} block size="lg" disabled={submitting} /></label>{/if}
          {#if categoriesShown}<label>Categories<TextInput ariaLabel="Organization categories" bind:value={categories} block size="lg" disabled={submitting} /></label>{/if}
        </div>{/if}
        {#if !aliasesShown || !categoriesShown}<div class="add-links">
          {#if !aliasesShown}<button type="button" class="add-link kit-control-states" disabled={submitting} onclick={() => addingAliases = true}>Add alias…</button>{/if}
          {#if !categoriesShown}<button type="button" class="add-link kit-control-states" disabled={submitting} onclick={() => addingCategories = true}>Add category…</button>{/if}
        </div>{/if}
        {#if aliasesShown || categoriesShown}<div class="list-save"><Button type="submit" label="Save aliases and categories" disabled={submitting} /></div>{/if}
      </form>{/if}
    {/if}
  </div>
{/if}
  {#snippet notice()}
    {#if attempt.conflict}
      <!-- The footer's Keep yours saves the organization's fields. -->
      {@render conflictNotice(attempt, undefined)}
    {:else if profileAttempt.conflict}
      {@render conflictNotice(profileAttempt, () => void saveProfile())}
    {:else if profileAttempt.details}
      <InlineNotice message="Couldn't save the aliases and categories." details={profileAttempt.details} actionLabel="Retry" onaction={() => void saveProfile()} />
    {:else if loadFailed}
      <InlineNotice message="Couldn't load this organization." details={load.details} actionLabel="Retry" onaction={() => void loadCurrent()} />
    {:else if attempt.details || controller.createBlocked.organizations}
      <InlineNotice message={controller.createBlocked.organizations ? "Couldn't confirm the organization was saved." : "Couldn't save this organization."} details={attempt.details || undefined} actionLabel={controller.createBlocked.organizations ? 'Check again' : 'Retry'} onaction={() => { if (controller.createBlocked.organizations) void refreshAfterUnknown(); else void saveOrganization(); }} />
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
  .editor .profile { margin-block: var(--space-group); }
  /* Each save's fields sit on the editor's own grid. */
  form { display: contents; }
  .list-save { display: flex; }
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
