<script lang="ts">
  import type { APIClient } from '../../api/client';
  import type { DirectoryReadBundle, DirectoryReadSection } from '../../directory/models';
  import type { DirectoryProfileController } from '../../directory/profile-controller.svelte';
  import type { DirectoryEntityController } from '../../directory/entity-controller.svelte';
  import FilesWorkspace from '../files/FilesWorkspace.svelte';
  import AttributeSection from './AttributeSection.svelte';
  import StructuredProfileSection from './StructuredProfileSection.svelte';
  import OrganizationEmploymentTab from './OrganizationEmploymentTab.svelte';
  import PersonNetwork from './PersonNetwork.svelte';
  import PersonMergeHistory from './PersonMergeHistory.svelte';
  import RelationshipsTab from './RelationshipsTab.svelte';
  import PersonTrackingControl from './PersonTrackingControl.svelte';
  import CardDAVPublicationControl from './CardDAVPublicationControl.svelte';
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
  }

  type DetailTab = 'overview' | 'organizations' | 'relationships' | 'network' | 'media';
  const tabOrder: DetailTab[] = ['overview', 'organizations', 'relationships', 'network', 'media'];
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
    onAnnounce = () => undefined
  }: Props = $props();
  let activeTab = $state<DetailTab>('overview');
  let organizationRequest = $state<{ id: number; key: number }>();
  let organizationRequestKey = 0;
  let overviewTab = $state<HTMLButtonElement>();
  let organizationsTab = $state<HTMLButtonElement>();
  let relationshipsTab = $state<HTMLButtonElement>();
  let networkTab = $state<HTMLButtonElement>();
  let mediaTab = $state<HTMLButtonElement>();
  const profile = $derived(bundle.structuredProfile);
  const overviewTabID = $derived(`person-${personID}-overview-tab`);
  const overviewPanelID = $derived(`person-${personID}-overview-panel`);
  const organizationsTabID = $derived(`person-${personID}-organizations-tab`);
  const organizationsPanelID = $derived(`person-${personID}-organizations-panel`);
  const relationshipsTabID = $derived(`person-${personID}-relationships-tab`);
  const relationshipsPanelID = $derived(`person-${personID}-relationships-panel`);
  const networkTabID = $derived(`person-${personID}-network-tab`);
  const networkPanelID = $derived(`person-${personID}-network-panel`);
  const mediaTabID = $derived(`person-${personID}-media-tab`);
  const mediaPanelID = $derived(`person-${personID}-media-panel`);
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

  function employmentOrganization(employmentID: number): string | undefined {
    const projection = entityController?.employmentProjection;
    return projection?.employment_id === employmentID ? projection.organization_name : undefined;
  }

  async function selectTab(tab: DetailTab, focus = false): Promise<void> {
    activeTab = tab;
    if (!focus) return;
    await Promise.resolve();
    ({ overview: overviewTab, organizations: organizationsTab, relationships: relationshipsTab, network: networkTab, media: mediaTab }[tab])?.focus();
  }

  function openOrganization(organizationID: number): void {
    organizationRequest = { id: organizationID, key: ++organizationRequestKey };
    void selectTab('organizations');
  }

  function handleTabKeydown(event: KeyboardEvent): void {
    let next: DetailTab | undefined;
    const index = tabOrder.indexOf(activeTab);
    if (event.key === 'ArrowRight') next = tabOrder[(index + 1) % tabOrder.length];
    else if (event.key === 'ArrowLeft') next = tabOrder[(index - 1 + tabOrder.length) % tabOrder.length];
    else if (event.key === 'Home') next = 'overview';
    else if (event.key === 'End') next = 'media';
    if (!next) return;
    event.preventDefault();
    void selectTab(next, true);
  }
</script>

<section class="person-detail" aria-label="Person detail">
  <div class="detail-tabs" role="tablist" aria-label="Person detail sections">
    <button bind:this={overviewTab} id={overviewTabID} type="button" role="tab"
      aria-selected={activeTab === 'overview'} aria-controls={overviewPanelID}
      tabindex={activeTab === 'overview' ? 0 : -1} onkeydown={handleTabKeydown}
      onclick={() => void selectTab('overview')}>Overview</button>
    <button bind:this={organizationsTab} id={organizationsTabID} type="button" role="tab"
      aria-selected={activeTab === 'organizations'} aria-controls={organizationsPanelID}
      tabindex={activeTab === 'organizations' ? 0 : -1} onkeydown={handleTabKeydown}
      onclick={() => void selectTab('organizations')}>Organizations</button>
    <button bind:this={relationshipsTab} id={relationshipsTabID} type="button" role="tab"
      aria-selected={activeTab === 'relationships'} aria-controls={relationshipsPanelID}
      tabindex={activeTab === 'relationships' ? 0 : -1} onkeydown={handleTabKeydown}
      onclick={() => void selectTab('relationships')}>Relationships</button>
    <button bind:this={networkTab} id={networkTabID} type="button" role="tab"
      aria-selected={activeTab === 'network'} aria-controls={networkPanelID}
      tabindex={activeTab === 'network' ? 0 : -1} onkeydown={handleTabKeydown}
      onclick={() => void selectTab('network')}>Network</button>
    <button bind:this={mediaTab} id={mediaTabID} type="button" role="tab"
      aria-selected={activeTab === 'media'} aria-controls={mediaPanelID}
      tabindex={activeTab === 'media' ? 0 : -1} onkeydown={handleTabKeydown}
      onclick={() => void selectTab('media')}>Media &amp; Files</button>
  </div>

  {#each Object.entries(bundle.errors) as [section, message]}
    <p class="section-error" role="alert">{sectionNames[section as DirectoryReadSection]}: {message}</p>
  {/each}

  {#if activeTab === 'media'}
    <div id={mediaPanelID} role="tabpanel" aria-labelledby={mediaTabID} tabindex="0">
      <!-- Durable Directory IDs use the People API, never the analytical participant route. -->
      <FilesWorkspace
        {client}
        identityScope={{ kind: 'durable-person', id: personID }}
        predicate={{ filters: [], presentation: 'files' }}
        sort={{ field: 'occurred_at', direction: 'desc' }}
        embedded
      />
    </div>
  {:else if activeTab === 'network'}
    <div id={networkPanelID} role="tabpanel" aria-labelledby={networkTabID} tabindex="0">
      {#if entityController}<PersonNetwork controller={entityController} {onOpenPerson} onOpenOrganization={openOrganization} />
      {:else}<section><h2>Network</h2><p>The curated network is unavailable for this selection.</p></section>{/if}
    </div>
  {:else if activeTab === 'relationships'}
    <div id={relationshipsPanelID} role="tabpanel" aria-labelledby={relationshipsTabID} tabindex="0">
      {#if entityController}<RelationshipsTab {client} controller={entityController} {personID} />
      {:else}<section><h2>Relationships</h2><p>Relationships are unavailable for this selection.</p></section>{/if}
    </div>
  {:else if activeTab === 'organizations'}
    <div id={organizationsPanelID} role="tabpanel" aria-labelledby={organizationsTabID} tabindex="0">
      {#if entityController}<OrganizationEmploymentTab controller={entityController} {personID} {organizationRequest} />
      {:else}<section><h2>Organizations</h2><p>Organizations are unavailable for this selection.</p></section>{/if}
    </div>
  {:else}
    <div id={overviewPanelID} role="tabpanel" aria-labelledby={overviewTabID} tabindex="0">
      {#if bundle.person || profile}
        <header><h2>{bundle.person?.display_name ?? profile?.person?.display_name ?? `Person ${personID}`}</h2></header>
      {/if}
      <PersonTrackingControl {client} {personID} {onAnnounce} />
      <CardDAVPublicationControl
        {client}
        {personID}
        onOpenConflict={onOpenCardDAVConflict}
        onOpenSettings={onOpenCardDAVSettings}
        {onAnnounce}
      />
      {#if profileController}
        <StructuredProfileSection {client} controller={profileController} {personID} />
      {:else if profile?.names?.length}
        <section><h3>Names</h3><ul>{#each profile.names as name}<li>{nameText(name)} <small>{name.name_kind}</small></li>{/each}</ul></section>
      {/if}
      {#if !profileController && groupedContacts().length}
        <section><h3>Contact observations</h3>{#each groupedContacts() as [service, points]}<h4>{service}</h4><ul>{#each points as point}<li>{point.original_value} <small>{point.address_kind}</small></li>{/each}</ul>{/each}</section>
      {/if}
      {#if !profileController && profile?.addresses?.length}
        <section><h3>Addresses</h3><ul>{#each profile.addresses as address}<li>{address.original_value} <small>{address.address_kind}</small></li>{/each}</ul></section>
      {/if}
      {#if !profileController && profile?.dates?.length}
        <section><h3>Dates</h3><ul>{#each profile.dates as date}<li>{date.label ?? date.date_kind}: {date.date_text ?? valueText(date.date)}</li>{/each}</ul></section>
      {/if}
      {#if !profileController && profile?.categories?.length}
        <section><h3>Categories</h3><ul>{#each profile.categories as category}<li>{category.original_value}</li>{/each}</ul></section>
      {/if}
      {#if profileController && profileController.attributes}
        <AttributeSection controller={profileController} />
      {:else if bundle.attributes?.attributes?.length}
        <section><h3>Attributes</h3><ul>{#each bundle.attributes.attributes as group}<li><strong>{group.definition.label}</strong>{#if group.definition.is_sensitive} <span class="sensitive">Sensitive</span>: concealed{:else}: {group.current?.map((value) => valueText(value.value)).join(', ')}{/if}</li>{/each}</ul></section>
      {/if}
      {#if entityController?.employments.length}
        <section><h3>Organizations and employment</h3><ul>{#each entityController.employments as employment}<li>{employment.title ?? employment.role ?? 'Employment'} · {employmentOrganization(employment.id) ?? `Organization ${employment.organization_id}`}{#if employment.is_current} <small>Current</small>{/if}</li>{/each}</ul></section>
      {/if}
      {#if entityController?.relationships.length}
        <section><h3>Relationships</h3><ul>{#each entityController.relationships as view}<li>{view.counterpart_display_name?.trim() || view.counterpart_vcard_uid || `Person ${view.counterpart_person_id}`} · {view.counterpart_label}</li>{/each}</ul></section>
      {/if}
      {#if bundle.contactState}
        <section><h3>Contact state</h3><p>{bundle.contactState.cadence_status} · {bundle.contactState.interaction_count} interactions{#if bundle.contactState.last_contact_at} · last contact {bundle.contactState.last_contact_at}{/if}</p></section>
      {/if}
      {#if bundle.activity}
        <section><h3>Activity</h3><p>{bundle.activity.total_count} recorded days</p></section>
      {/if}
      <PersonMergeHistory {client} {personID} {onOpenPerson} {onSplitCommitted} />
    </div>
  {/if}
</section>

<style>
  .person-detail { padding: var(--space-4); display: grid; gap: var(--space-4); }
  .detail-tabs { display: flex; gap: var(--space-2); }
  [role="tabpanel"] { display: grid; gap: var(--space-4); outline: none; }
  [role="tab"] { border: 1px solid var(--border-default); border-radius: var(--radius-sm); padding: var(--space-2) var(--space-3); background: var(--bg-inset); color: var(--text-secondary); cursor: pointer; }
  [role="tab"][aria-selected="true"] { background: var(--bg-surface-hover); color: var(--text-primary); }
  section { display: grid; gap: var(--space-2); }
  h2, h3, h4, p, ul { margin: 0; }
  h3 { font-size: var(--font-size-md); } h4, small { color: var(--text-muted); font-size: var(--font-size-sm); }
  ul { padding-left: var(--space-5); }
  .section-error { margin: 0; padding: var(--space-2); background: var(--bg-inset); color: var(--text-secondary); }
  .sensitive { display: inline-block; padding: 1px 5px; border-radius: var(--radius-sm); background: var(--bg-warning); color: var(--text-primary); font-size: var(--font-size-sm); }
</style>
