<script lang="ts">
  import { Button, Card } from '@kenn-io/kit-ui';
  import { onDestroy, tick, untrack } from 'svelte';
  import type { MeetingRef } from '../../api/generated/models';
  import MeetingPanel from '../meetings/MeetingPanel.svelte';
  import type { APIClient } from '../../api/client';
  import type { DirectoryReadBundle, DirectoryReadSection } from '../../directory/models';
  import type { DirectoryProfileController } from '../../directory/profile-controller.svelte';
  import type { DirectoryEntityController } from '../../directory/entity-controller.svelte';
  import FilesWorkspace from '../files/FilesWorkspace.svelte';
  import AttributeSection from './AttributeSection.svelte';
  import AttributeSummary from './AttributeSummary.svelte';
  import StructuredProfileSection from './StructuredProfileSection.svelte';
  import OrganizationEmploymentTab from './OrganizationEmploymentTab.svelte';
  import PersonNetwork from './PersonNetwork.svelte';
  import PersonMergeHistory from './PersonMergeHistory.svelte';
  import RelationshipsTab from './RelationshipsTab.svelte';
  import PersonTrackingControl from './PersonTrackingControl.svelte';
  import PersonBriefCard from './PersonBriefCard.svelte';
  import PersonAgenda from './PersonAgenda.svelte';
  import CardDAVPublicationControl from './CardDAVPublicationControl.svelte';
  import PersonRecordActions from './PersonRecordActions.svelte';
  import { contactStateLabel, formatContactDate } from '../../directory/labels';
  import type { FileMIMEFamily, FileSearchSort } from '../../explore/models';
  import { bufferedCallback } from '../../util/buffered-callback';
  import type { PersonSplitCommittedContext } from '../../directory/person-merge-history-controller.svelte';

  interface Props {
    client: APIClient;
    bundle: DirectoryReadBundle;
    personID: number;
    profileController?: DirectoryProfileController | null;
    entityController?: DirectoryEntityController | null;
    onOpenPerson?: (personID: number) => void;
    onSplitCommitted?: (context: PersonSplitCommittedContext) => void | Promise<void>;
    onOpenCardDAVConflict?: (conflictID: number) => void;
    onOpenCardDAVSettings?: () => void;
    onAnnounce?: (message: string) => void;
    onOpenMeeting?: (meeting: MeetingRef) => void;
    onOpenRelationship?: (participantID: number) => void;
    onReviewFacts?: (personID: number) => void;
  }

  type DetailTab = 'overview' | 'profile' | 'organizations' | 'connections' | 'network' | 'media' | 'maintenance';
  const SECTIONS: ReadonlyArray<{ id: DetailTab; label: string; description: string }> = [
    { id: 'overview', label: 'Overview', description: 'Activity, agenda, attributes, and meeting context.' },
    { id: 'profile', label: 'Profile', description: 'Names, contact points, addresses, dates, and categories.' },
    { id: 'organizations', label: 'Organizations', description: 'Employment history and organization records.' },
    { id: 'connections', label: 'Connections', description: 'Curated relationships with other people.' },
    { id: 'network', label: 'Network', description: 'A graph of durable people, organizations, and connections.' },
    { id: 'media', label: 'Media & files', description: 'Files associated with this durable person.' },
    { id: 'maintenance', label: 'Maintenance', description: 'Tracking, CardDAV publication, and merge history.' },
  ];
  let {
    client,
    bundle,
    personID,
    profileController = null,
    entityController = null,
    onOpenPerson = () => undefined,
    onSplitCommitted = () => undefined,
    onOpenCardDAVConflict = () => undefined,
    onOpenCardDAVSettings = () => undefined,
    onAnnounce = () => undefined,
    onOpenMeeting = undefined,
    onOpenRelationship = undefined,
    onReviewFacts = undefined
  }: Props = $props();
  let activeTab = $state<DetailTab>('overview');
  const activeSection = $derived(SECTIONS.find((section) => section.id === activeTab) ?? SECTIONS[0]!);
  let fileSort = $state<FileSearchSort>({ field: 'occurred_at', direction: 'desc' });
  let fileFilenameQuery = $state('');
  let fileMIMEFamilies = $state<FileMIMEFamily[]>([]);
  const FILENAME_DEBOUNCE_MS = 250;
  const debouncedFilenameQuery = bufferedCallback((value: string) => { fileFilenameQuery = value; }, FILENAME_DEBOUNCE_MS);
  onDestroy(debouncedFilenameQuery.cancel);
  let filesPersonID = untrack(() => personID);
  $effect(() => {
    if (personID === filesPersonID) return;
    filesPersonID = personID;
    debouncedFilenameQuery.cancel();
    fileSort = { field: 'occurred_at', direction: 'desc' };
    fileFilenameQuery = '';
    fileMIMEFamilies = [];
  });
  let organizationRequest = $state<{ id: number; key: number }>();
  let organizationRequestKey = 0;
  const tabButtons: Partial<Record<DetailTab, HTMLButtonElement>> = $state({});
  const tabID = (tab: DetailTab) => `person-${personID}-${tab}-tab`;
  const panelID = (tab: DetailTab) => `person-${personID}-${tab}-panel`;
  const profile = $derived(bundle.structuredProfile);
  const displayName = $derived(bundle.person?.display_name ?? profile?.person?.display_name ?? `Person ${personID}`);
  const participantID = $derived(
    bundle.person?.participant_ids.length ? Math.min(...bundle.person.participant_ids) : undefined
  );
  const sectionNames: Record<DirectoryReadSection, string> = {
    person: 'Person', structuredProfile: 'Profile', attributes: 'Attributes', contactState: 'Contact state',
    activity: 'Activity', files: 'Files'
  };

  function valueText(value: unknown): string {
    if (typeof value === 'string' || typeof value === 'number') return String(value);
    return JSON.stringify(value);
  }

  function nameText(name: NonNullable<NonNullable<DirectoryReadBundle['structuredProfile']>['names']>[number]): string {
    return name.formatted ?? ([name.given_name, name.family_name].filter(Boolean).join(' ') || name.original_value);
  }

  function groupedContacts() {
    const groups = new Map<string, NonNullable<DirectoryReadBundle['structuredProfile']>['contact_points']>();
    for (const point of profile?.contact_points ?? []) {
      const service = point.service_slug ?? 'other';
      groups.set(service, [...(groups.get(service) ?? []), point]);
    }
    return [...groups.entries()];
  }

  function contactStateText(state: NonNullable<DirectoryReadBundle['contactState']>): string {
    const parts = [contactStateLabel(state.cadence_status), `${state.interaction_count} interactions`];
    if (state.last_contact_at) parts.push(`last contact ${formatContactDate(state.last_contact_at)}`);
    return parts.join(' · ');
  }

  function employmentOrganization(employmentID: number): string | undefined {
    const projection = entityController?.employmentProjection;
    return projection?.employment_id === employmentID ? projection.organization_name : undefined;
  }

  async function selectTab(tab: DetailTab, focus = false): Promise<void> {
    activeTab = tab;
    if (!focus) return;
    await Promise.resolve();
    tabButtons[tab]?.focus();
  }

  function openOrganization(organizationID: number): void {
    organizationRequest = { id: organizationID, key: ++organizationRequestKey };
    void selectTab('organizations');
  }

  function handleTabKeydown(event: KeyboardEvent): void {
    let next: DetailTab | undefined;
    const tabOrder = SECTIONS.map((section) => section.id);
    const index = tabOrder.indexOf(activeTab);
    if (event.key === 'ArrowRight') next = tabOrder[(index + 1) % tabOrder.length];
    else if (event.key === 'ArrowLeft') next = tabOrder[(index - 1 + tabOrder.length) % tabOrder.length];
    else if (event.key === 'Home') next = 'overview';
    else if (event.key === 'End') next = 'maintenance';
    if (!next) return;
    event.preventDefault();
    void selectTab(next, true);
  }
</script>

<section class="person-detail" aria-label="Person detail">
  <header class="person-header">
    <h2>{displayName}</h2>
    <div class="person-actions">
      {#if participantID !== undefined && onOpenRelationship}
        <Button label="Open relationship" surface="outline" size="sm" onclick={() => onOpenRelationship(participantID)} />
      {/if}
      {#if onReviewFacts}
        <Button label="Review facts" surface="outline" size="sm" onclick={() => onReviewFacts(personID)} />
      {/if}
      {#if profileController}<PersonRecordActions {client} controller={profileController} {personID} />{/if}
    </div>
  </header>

  <Card level="default" padding="none" class="detail-surface">
    <div class="detail-frame">
      <div class="detail-tabs" role="tablist" aria-label="Person detail sections" data-scroll>
        {#each SECTIONS as section (section.id)}
          <button bind:this={tabButtons[section.id]} id={tabID(section.id)} type="button" role="tab"
            aria-selected={activeTab === section.id} aria-controls={panelID(section.id)}
            tabindex={activeTab === section.id ? 0 : -1} onkeydown={handleTabKeydown}
            onclick={() => void selectTab(section.id)}>{section.label}</button>
        {/each}
      </div>

      <div class="panel-shell" id={panelID(activeTab)} role="tabpanel" aria-labelledby={tabID(activeTab)} tabindex="0">
        <header class="panel-heading">
          <span>Person record</span>
          <h3>{activeSection.label}</h3>
          <p>{activeSection.description}</p>
        </header>

        {#each Object.entries(bundle.errors) as [section, message]}
          <p class="section-error" role="alert">{sectionNames[section as DirectoryReadSection]}: {message}</p>
        {/each}

        <div class="tab-body">
    {#if activeTab === 'overview'}
      <PersonBriefCard {client} {personID} {onAnnounce} />
      <PersonAgenda {client} {personID} {onAnnounce} />
      <AttributeSummary
        groups={profileController?.attributes?.attributes ?? bundle.attributes?.attributes ?? []}
        onEdit={profileController ? async () => {
          await selectTab('profile');
          await tick();
          const section = document.getElementById('person-attributes');
          section?.scrollIntoView({ block: 'start', behavior: 'smooth' });
          section?.focus({ preventScroll: true });
        } : undefined}
      />
      {#if bundle.contactState}
        <section><h3>Contact state</h3><p>{contactStateText(bundle.contactState)}</p></section>
      {/if}
      {#if bundle.activity}
        <section><h3>Activity</h3><p>{bundle.activity.total_count} recorded days</p></section>
      {/if}
      {#if entityController?.employments.length}
        <section><h3>Organizations and employment</h3><ul>{#each entityController.employments as employment}<li>{employment.title ?? employment.role ?? 'Employment'} · {employmentOrganization(employment.id) ?? `Organization ${employment.organization_id}`}{#if employment.is_current} <small>Current</small>{/if}</li>{/each}</ul></section>
      {/if}
      {#if entityController?.relationships.length}
        <section><h3>Connections</h3><ul>{#each entityController.relationships as view}<li>{view.counterpart_display_name?.trim() || view.counterpart_vcard_uid || `Person ${view.counterpart_person_id}`} · {view.counterpart_label}</li>{/each}</ul></section>
      {/if}
      {#if bundle.person?.id === personID}
        <MeetingPanel {client} scope={{ kind: 'direct', scope: { person_id: personID } }}
          refreshKey={JSON.stringify([bundle.person.revision, [...bundle.person.participant_ids].sort((a, b) => a - b)])}
          {onOpenMeeting} />
      {/if}
    {:else if activeTab === 'profile'}
      {#if profileController}
        <StructuredProfileSection {client} controller={profileController} {personID} />
      {:else}
        {#if profile?.names?.length}
          <section><h3>Names</h3><ul>{#each profile.names as name}<li>{nameText(name)} <small>{name.name_kind}</small></li>{/each}</ul></section>
        {/if}
        {#if groupedContacts().length}
          <section><h3>Contact observations</h3>{#each groupedContacts() as [service, points]}<h4>{service}</h4><ul>{#each points as point}<li>{point.original_value} <small>{point.address_kind}</small></li>{/each}</ul>{/each}</section>
        {/if}
        {#if profile?.addresses?.length}
          <section><h3>Addresses</h3><ul>{#each profile.addresses as address}<li>{address.original_value} <small>{address.address_kind}</small></li>{/each}</ul></section>
        {/if}
        {#if profile?.dates?.length}
          <section><h3>Dates</h3><ul>{#each profile.dates as date}<li>{date.label ?? date.date_kind}: {date.date_text ?? valueText(date.date)}</li>{/each}</ul></section>
        {/if}
        {#if profile?.categories?.length}
          <section><h3>Categories</h3><ul>{#each profile.categories as category}<li>{category.original_value}</li>{/each}</ul></section>
        {/if}
      {/if}
      {#if profileController && profileController.attributes}
        <AttributeSection controller={profileController} />
      {/if}
    {:else if activeTab === 'organizations'}
      {#if entityController}<OrganizationEmploymentTab controller={entityController} {personID} {organizationRequest} />
      {:else}<section><h3>Organizations</h3><p>Organizations are unavailable for this selection.</p></section>{/if}
    {:else if activeTab === 'connections'}
      {#if entityController}<RelationshipsTab {client} controller={entityController} {personID} />
      {:else}<section><h3>Connections</h3><p>Connections are unavailable for this selection.</p></section>{/if}
    {:else if activeTab === 'network'}
      {#if entityController}<PersonNetwork controller={entityController} {onOpenPerson} onOpenOrganization={openOrganization} />
      {:else}<section><h3>Network</h3><p>The curated network is unavailable for this selection.</p></section>{/if}
    {:else if activeTab === 'media'}
      <!-- Durable Directory IDs use the People API, never the analytical participant route. -->
      <FilesWorkspace
        {client}
        identityScope={{ kind: 'durable-person', id: personID }}
        predicate={{ filters: [], presentation: 'files' }}
        sort={fileSort}
        filenameQuery={fileFilenameQuery}
        mimeFamilies={fileMIMEFamilies}
        onSortChange={(value) => (fileSort = value)}
        onFilenameQueryChange={debouncedFilenameQuery}
        onMIMEFamiliesChange={(value) => (fileMIMEFamilies = value)}
        embedded
      />
    {:else}
      <PersonTrackingControl {client} {personID} {onAnnounce} />
      <CardDAVPublicationControl
        {client}
        {personID}
        onOpenConflict={onOpenCardDAVConflict}
        onOpenSettings={onOpenCardDAVSettings}
        {onAnnounce}
      />
      <PersonMergeHistory {client} {personID} {onOpenPerson} {onSplitCommitted} />
    {/if}
        </div>
      </div>
    </div>
  </Card>
</section>

<style>
  .person-detail { display: grid; min-width: 0; gap: var(--space-4); padding: var(--space-4); }
  .person-header { display: flex; flex-wrap: wrap; align-items: center; gap: var(--space-2); }
  .person-header h2 { flex: 1 1 auto; min-width: 0; overflow-wrap: anywhere; }
  .person-actions { display: contents; }
  .person-detail :global(.detail-surface) { min-width: 0; overflow: hidden; }
  .detail-frame { min-width: 0; }
  .detail-tabs { display: flex; flex-wrap: nowrap; gap: var(--space-1); overflow-x: auto; padding: var(--space-2); border-bottom: 1px solid var(--border-muted); background: var(--bg-inset); }
  [role="tab"] { flex: none; padding: var(--space-2) var(--space-3); border: 0; border-radius: var(--radius-sm); background: transparent; color: var(--text-muted); font: inherit; font-size: var(--font-size-sm); font-weight: 500; white-space: nowrap; cursor: pointer; }
  [role="tab"]:hover { background: var(--bg-surface-hover); color: var(--text-secondary); }
  [role="tab"][aria-selected="true"] { background: var(--bg-surface); color: var(--text-primary); box-shadow: var(--shadow-sm); font-weight: 650; }
  [role="tab"]:focus-visible { outline: var(--focus-ring); outline-offset: -2px; }
  .panel-shell { display: grid; min-width: 0; gap: var(--space-5); padding: var(--space-5); background: var(--bg-surface); }
  .panel-shell:focus-visible { outline: var(--focus-ring); outline-offset: -3px; }
  .panel-heading { display: grid; gap: var(--space-1); padding-bottom: var(--space-4); border-bottom: 1px solid var(--border-muted); }
  .panel-heading span { color: var(--text-muted); font-size: var(--font-size-2xs); font-weight: 600; letter-spacing: .04em; text-transform: uppercase; }
  .panel-heading h3 { font-size: var(--font-size-lg); }
  .panel-heading p { color: var(--text-muted); font-size: var(--font-size-sm); }
  .tab-body { display: grid; min-width: 0; gap: var(--space-4); }
  section { display: grid; gap: var(--space-2); }
  h2, h3, h4, p, ul { margin: 0; }
  h3 { font-size: var(--font-size-md); } h4, small { color: var(--text-muted); font-size: var(--font-size-sm); }
  ul { padding-left: var(--space-5); }
  .section-error { margin: 0; padding: var(--space-3); border-radius: var(--radius-sm); background: var(--bg-inset); color: var(--text-secondary); }
  @media (max-width: 640px) {
    .person-detail { padding: var(--space-3); }
    .panel-shell { padding: var(--space-4); }
  }
</style>
