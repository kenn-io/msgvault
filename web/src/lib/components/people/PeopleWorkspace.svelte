<script lang="ts">
  import { appShortcuts, ROOT_SCOPE, Button, Modal, PageHeader, TextInput, SelectDropdown, type FilterDropdownSection } from '@kenn-io/kit-ui';
  import { deletePerson } from '../../api/generated/api/api';
  import { onMount, tick, untrack } from 'svelte';
  import type { RelationshipsController, RelationshipsMergeContext } from '../../relationships/controller.svelte';
  import type { DirectoryController } from '../../directory/controller.svelte';
  import { PRIMARY_CHANNELS, channelLabel, contactStateLabel, formatDay } from '../../directory/labels';
  import type { RelationshipRow } from '../../relationships/models';
  import type { DomainSummary, PersonSummary, ExploreURLState, FileSearchSort, FileMIMEFamily } from '../../explore/models';
  import { ExploreSelectionState } from '../../explore/state.svelte';
  import type { ValidatedPersonMergeRequired, PersonMergeSuccess } from '../../directory/person-merge';
  import PersonBindingConflictModal from '../directory/PersonBindingConflictModal.svelte';
  import SplitPane from '../layout/SplitPane.svelte';
  import PeopleList, { type PeopleRow } from './PeopleList.svelte';
  import { messageCount } from '../../util/format';
  import PossibleDuplicates from './PossibleDuplicates.svelte';
  import type { DirectoryReviewController } from '../../directory/review-controller.svelte';
  import PersonPage from './PersonPage.svelte';
  import { RELATIONSHIP_DAY_HISTORY_KEY, relationshipDayHistory } from '../../relationships/reader-history';
  import type { ComponentProps } from 'svelte';

  type PageProps = Omit<ComponentProps<typeof PersonPage>, 'directory' | 'onRecordDeleted' | 'onSplitCommitted' | 'onIdentityChange' | 'onBack' | 'onReconcilePersonMerge' | 'details' | 'selectedDate' | 'domainYear' | 'sort' | 'filenameQuery' | 'mimeFamilies'>;
  let { reviews, onDuplicatesChanged, directory, urlState, restorationEpoch = 0, onNavigate, onReplaceNavigation, onDraftNavigation, onShowAllChange, ...page }:
    PageProps & { reviews: DirectoryReviewController; onDuplicatesChanged: () => void; directory: DirectoryController; urlState: ExploreURLState; restorationEpoch?: number; onReplaceNavigation: (patch: Partial<ExploreURLState>) => void;
      onNavigate: (patch: Partial<ExploreURLState>) => void; onDraftNavigation: (patch: Partial<ExploreURLState>) => void; onShowAllChange: (value: boolean) => void } = $props();
  const query = $derived(urlState.identityQuery ?? '');
  const controller = $derived(page.controller);
  const facet = $derived(urlState.relationshipFacet);
  const showAll = $derived(urlState.relationshipShowAll);
  const selection = new ExploreSelectionState();
  let root = $state<HTMLElement>();
  let narrow = $state(false);
  let details = $state(false);
  let selectedDate = $state<string | null>(null);
  let domainYear = $state<number | null>(null);
  let fileSort = $state<FileSearchSort>({ field: 'occurred_at', direction: 'desc' });
  let filenameQuery = $state('');
  let mimeFamilies = $state<FileMIMEFamily[]>([]);
  const target = $derived(page.target);
  // Each history entry keeps its own day: Back restores that entry's day, even for the same target.
  $effect(() => { void page.target; untrack(() => selectedDate = relationshipDayHistory(window.history.state, 'relationships', page.target)); });
  $effect(() => { const target = untrack(() => page.target); window.history.replaceState({ ...window.history.state, [RELATIONSHIP_DAY_HISTORY_KEY]: selectedDate && target ? { target, date: selectedDate } : undefined }, ''); });
  $effect(() => { void (selectedProfileID ?? target); untrack(() => { domainYear = null; details &&= controller.target === target && controller.detail !== null; filenameQuery = ''; mimeFamilies = []; }); });
  const saved = $derived(urlState.relationshipSaved);
  let filterOpen = $state(false);
  const sort = $derived(urlState.relationshipSort);
  let confirmDelete = $state(false);
  $effect(() => { if (confirmDelete) return appShortcuts.pushScope('people-bulk-delete'); });
  const selectedSaved = $derived(directory.rows.filter(person => selection.isSelected(`person:${person.id}`)));
  let bulkPending = $state(false);
  let bulkMessage = $state<string | null>(null);
  let bulkConflict = $state<ValidatedPersonMergeRequired>();
  let bulkIDs = $state<number[]>([]);
  let bulkCompleted = $state(0);
  let bulkContext = $state<ReturnType<RelationshipsController['personMergeContextSnapshot']>>();
  let staleLink = $state<{ a: number; b: number } | null>(null);
  const profileFilters = $derived(Boolean(urlState.directoryContactState || urlState.directoryCategory || urlState.directoryOrganization || urlState.directoryPrimaryChannel || urlState.directoryLastContactAfter || urlState.directoryLastContactBefore));
  const directoryMode = $derived(facet === 'people' && (saved || profileFilters));
  const rankedList = $derived(facet === 'people' && !directoryMode && sort === 'closest' && query.trim() === '');
  const soloPerson = $derived(urlState.directoryPersonID);
  const predicateKey = $derived(JSON.stringify(page.predicate));
  // The directory outlives this view, so a new mount reloads its retained rows once.
  let appliedRestorationEpoch: number | undefined;
  $effect(() => {
    controller.facet = facet; controller.showAll = showAll; controller.query = query;
    controller.sort = sort === 'name' ? { field: 'display_label', direction: 'asc' } : sort === 'last_contact_asc' ? { field: 'latest_at', direction: 'asc' } : sort === 'recent' ? { field: 'latest_at', direction: 'desc' } : { field: 'activity_count', direction: 'desc' };
    void predicateKey;
    if (!directoryMode && !urlState.peopleDuplicates) untrack(() => void controller.loadList(page.predicate));
  });
  $effect(() => {
    if (!directoryMode || urlState.peopleDuplicates) return;
    const filters = { ...urlState, directoryQuery: query, directorySort: sort === 'last_contact_asc' ? 'last_contact_asc' : sort === 'recent' ? 'last_contact_desc' : 'name' } as const;
    untrack(() => directory.applyURLState(filters, appliedRestorationEpoch !== (appliedRestorationEpoch = restorationEpoch), false));
  });
  $effect(() => { void [facet, query, sort, directoryMode, predicateKey, showAll, urlState.directoryContactState, urlState.directoryCategory, urlState.directoryOrganization, urlState.directoryPrimaryChannel, urlState.directoryLastContactAfter, urlState.directoryLastContactBefore]; untrack(() => selection.clear()); });
  onMount(() => {
    const observer = new ResizeObserver(([entry]) => narrow = (entry?.contentRect.width ?? 1200) < 720);
    if (root) observer.observe(root);
    return () => { observer.disconnect(); directory.resetForPromotion(); };
  });
  const rows = $derived.by((): PeopleRow[] => directoryMode ? directory.rows.map((person) => ({
    key: `person:${person.id}`, label: person.display_name || `Person ${person.id}`, detail: person.organizations.join(', '), lastAt: person.last_contact_at ?? '', saved: true, domain: false,
  })) : (controller.listRows as Array<RelationshipRow | PersonSummary | DomainSummary>).map((row) => {
    if ('canonical_id' in row) {
      const person = row as RelationshipRow;
      return { key: `cluster:${person.canonical_id}`, label: person.display_label, detail: `${person.signals.sent_count.toLocaleString()} from you`, lastAt: person.signals.last_interaction_at, saved: false, domain: false };
    }
    if ('domain' in row) {
      const domain = row as DomainSummary;
      return { key: `domain:${domain.domain}`, label: domain.domain, detail: messageCount(domain.activity_count), lastAt: domain.last_at, saved: false, domain: true };
    }
    const person = row as PersonSummary;
    return { key: `cluster:${person.id}`, label: person.display_label, detail: messageCount(person.activity_count), lastAt: person.last_at, saved: Boolean(person.profile), domain: false };
  }));
  const loading = $derived(directoryMode ? directory.loading : controller.listLoading);
  const hasMore = $derived(Boolean(directoryMode ? directory.cursor : controller.listCursor));
  const activeKey = $derived(soloPerson !== null ? `person:${soloPerson}` : page.target);
  const hasPerson = $derived(page.target !== null || soloPerson !== null);
  $effect(() => {
    if (narrow && activeKey) void tick().then(() => root?.querySelector<HTMLElement>('.body > .solo-page')?.focus());
  });
  const selectedProfileID = $derived(soloPerson ?? (controller.target === target && controller.detail && 'identifiers' in controller.detail ? (controller.detail as PersonSummary).profile?.id ?? null : null));
  $effect(() => {
    const id = selectedProfileID;
    if (id === null) void target;
    untrack(() => { if (id === null || directory.selectedPersonID !== id) void directory.loadSelection(id); });
  });
  $effect(() => {
    const id = soloPerson, person = directory.detail?.person;
    if (id === null || directory.selectedPersonID !== id || !person) return;
    const targets = person.participant_ids.map((participant) => `cluster:${participant}`);
    const next = targets.includes(page.target ?? '') ? page.target : targets[0] ?? null;
    if (next !== page.target) onReplaceNavigation({ relationshipTarget: next });
  });
  async function open(key: string): Promise<void> {
    onNavigate({ directoryPersonID: key.startsWith('person:') ? Number(key.slice(7)) : null, relationshipTarget: key.startsWith('person:') ? null : key, relationshipFiles: false });
  }
  function escape(event: KeyboardEvent): void {
    if (event.key !== 'Escape' || !hasPerson || urlState.peopleDuplicates || appShortcuts.activeScope() !== ROOT_SCOPE) return;
    if ((event.target as Element | null)?.closest('input, textarea, select, [contenteditable]') || document.querySelector('.kit-popover-card')) return;
    close();
    event.preventDefault();
    event.stopPropagation();
  }
  function close(): void {
    onNavigate({ directoryPersonID: null, relationshipTarget: null, relationshipFiles: false });
    void tick().then(() => root?.querySelector<HTMLElement>('[aria-label="People results"]')?.focus());
  }
  function reset(): void { onNavigate({ relationshipSaved: false, relationshipShowAll: false, directoryContactState: '', directoryCategory: '', directoryOrganization: '', directoryPrimaryChannel: '', directoryLastContactAfter: '', directoryLastContactBefore: '' }); }
  const filterSections = $derived<FilterDropdownSection[]>([{ items: [
    { id: 'duplicates', active: false, label: 'Possible duplicates', onSelect: () => onNavigate({ peopleDuplicates: true, identityState: 'candidate' }) },
    { id: 'saved', label: 'Saved', active: saved, closeOnSelect: false, onSelect: () => onNavigate({ relationshipSaved: !saved }) },
    ...(rankedList ? [{ id: 'one-way', label: 'Include one-way senders', active: showAll, closeOnSelect: false, onSelect: () => onShowAllChange(!showAll) }] : []),
    { id: 'profile', label: 'Profile filters', active: profileFilters, onSelect: () => filterOpen = !filterOpen },
  ] }]);
  const chips = $derived(facet === 'people' ? [
    ...(saved ? [{ id: 'saved', label: 'Saved', onremove: () => onNavigate({ relationshipSaved: false }) }] : []),
    ...(showAll && rankedList ? [{ id: 'one-way', label: 'Including one-way senders', onremove: () => onShowAllChange(false) }] : []),
    ...(['directoryContactState', 'directoryCategory', 'directoryOrganization', 'directoryPrimaryChannel', 'directoryLastContactAfter', 'directoryLastContactBefore'] as const)
      .filter((key) => urlState[key]).map((key) => ({ id: key, label: key === 'directoryContactState' ? contactStateLabel(urlState[key]) : key === 'directoryPrimaryChannel' ? channelLabel(urlState[key]) : key === 'directoryLastContactAfter' ? `Last contacted after ${formatDay(urlState[key])}` : key === 'directoryLastContactBefore' ? `Last contacted before ${formatDay(urlState[key])}` : urlState[key], onremove: () => onNavigate({ [key]: '' }) })),
  ] : []);
  function loadMore(): void { if (directoryMode) void directory.loadNextPage(); else void controller.loadMoreList(); }
  async function deleteSelected(): Promise<void> {
    if (bulkPending || selectedSaved.length === 0) return;
    bulkPending = true;
    const deleted: number[] = [], failures: string[] = [];
    for (const profile of selectedSaved) {
      const id = profile.id;
      try {
        const result = await deletePerson({ id }, { ...page.client, headers: { 'If-Match': `"person-${id}-r${profile.revision}"` } });
        if (result.response.status === 204) { deleted.push(id); selection.explicitKeys.delete(`person:${id}`); }
        else failures.push(`${profile.display_name || `Person ${id}`}: ${result.error?.message ?? 'Could not delete.'}`);
      } catch (cause) { failures.push(`${profile.display_name || `Person ${id}`}: ${cause instanceof Error ? cause.message : 'Could not delete.'}`); }
    }
    await directory.removeDeletedPeople(deleted);
    await peopleDeleted(deleted);
    bulkMessage = `${deleted.length} deleted.${failures.length ? ` ${failures.join(' ')}` : ''}`;
    bulkPending = false; confirmDelete = false;
  }
  async function peopleDeleted(ids: number[]): Promise<void> {
    onDuplicatesChanged();
    if (selectedProfileID !== null && ids.includes(selectedProfileID)) close();
  }
  async function startBulk(): Promise<void> {
    const ids = [...selection.explicitKeys].map((key) => /^cluster:([1-9][0-9]*)$/.exec(key)?.[1]).filter((id): id is string => id !== undefined).map(Number);
    if (ids.length < 2 || bulkPending || bulkConflict) return;
    bulkIDs = ids; bulkCompleted = 0; bulkContext = controller.personMergeContextSnapshot();
    await continueBulk();
  }
  async function continueBulk(): Promise<void> {
    if (bulkIDs.length < 2) return;
    const { controller } = page;
    bulkPending = true;
    try {
      while (bulkCompleted < bulkIDs.length - 1) {
        const next = bulkIDs[bulkCompleted + 1]!;
        const result = await controller.linkParticipants(bulkIDs[0]!, next);
        if (result.ok || result.code === 'already_linked') {
          if (result.ok) { onDuplicatesChanged(); staleLink = result.cacheState === 'stale' ? { a: bulkIDs[0]!, b: next } : null; await directory.refreshSelectedPerson(); }
          bulkCompleted += 1; continue;
        }
        if (result.code === 'merge_required') { bulkConflict = result.conflict; return; }
        bulkMessage = `${bulkCompleted} of ${bulkIDs.length - 1} linked. ${result.message}`; return;
      }
      selection.clear();
      bulkMessage = staleLink ? 'People linked. The identity cache needs a retry.' : 'People are now treated as the same person.';
    } finally {
      bulkPending = false;
      // Linking moves activity between people, even when navigation has unmounted this workspace.
      if (bulkCompleted > 0) await directory.reloadFirstPage();
    }
  }
  async function refreshProfiles(): Promise<void> {
    onDuplicatesChanged();
    await directory.reloadFirstPage();
    if (directory.selectedPersonID === selectedProfileID) await directory.refreshSelectedPerson();
  }
  async function reconcilePersonMerge(context: RelationshipsMergeContext, success: PersonMergeSuccess | undefined = undefined): Promise<void> {
    if (success && soloPerson === success.result.merge.absorbed_person_id) {
      onReplaceNavigation({ directoryPersonID: success.survivor.id });
    }
    await Promise.all([controller.reconcilePersonMerge(context), refreshProfiles()]);
  }
  async function merged(success: PersonMergeSuccess): Promise<void> {
    bulkPending = true; bulkConflict = undefined;
    if (soloPerson === success.result.merge.absorbed_person_id) onReplaceNavigation({ directoryPersonID: success.survivor.id });
    page.onAnnounce?.(`Profiles merged into ${success.survivor.display_name}.`);
    if (bulkContext) await reconcilePersonMerge(bulkContext);
    await continueBulk();
  }
  async function retryLink(): Promise<void> {
    if (!staleLink || bulkPending) return;
    bulkPending = true; const result = await controller.linkParticipants(staleLink.a, staleLink.b); bulkPending = false;
    if (result.ok) onDuplicatesChanged();
    if (result.ok && result.cacheState === 'ready') { staleLink = null; if (bulkCompleted === bulkIDs.length - 1) bulkMessage = 'Identity cache refreshed.'; }
  }
</script>

{#snippet list()}
  <div class="list">
    {#if filterOpen && facet === 'people'}
      <div class="filters" role="group" aria-label="Profile filters">
        <SelectDropdown title="Contact state" value={urlState.directoryContactState} options={[{ value: '', label: 'Any status' }, { value: 'active', label: 'Active' }, { value: 'inactive', label: 'Inactive' }]} onchange={(value) => onNavigate({ directoryContactState: value })} />
        <TextInput ariaLabel="Category filter" placeholder="Category" value={urlState.directoryCategory} oninput={(value) => onDraftNavigation({ directoryCategory: value })} />
        <TextInput ariaLabel="Organization filter" placeholder="Organization" value={urlState.directoryOrganization} oninput={(value) => onDraftNavigation({ directoryOrganization: value })} />
        <SelectDropdown title="Primary channel" value={urlState.directoryPrimaryChannel} options={[{ value: '', label: 'All channels' }, ...PRIMARY_CHANNELS.map((value) => ({ value, label: channelLabel(value) }))]} onchange={(value) => onNavigate({ directoryPrimaryChannel: value })} />
        <label>Last contacted after
          <!-- kit-ui-check-ignore: retain the existing Directory optional one-sided date control. -->
          <input type="date" value={urlState.directoryLastContactAfter} onchange={(event) => onNavigate({ directoryLastContactAfter: event.currentTarget.value })} /></label>
        <label>Last contacted before
          <!-- kit-ui-check-ignore: retain the existing Directory optional one-sided date control. -->
          <input type="date" value={urlState.directoryLastContactBefore} onchange={(event) => onNavigate({ directoryLastContactBefore: event.currentTarget.value })} /></label>
      </div>
    {/if}
    <div inert={bulkPending || Boolean(bulkConflict)} class="list-content">
      <PeopleList pageRecovery={directoryMode ? directory.pageRecovery : null} {rows} {loading} loadingMore={directoryMode ? directory.loadingMore : controller.listLoadingMore} {hasMore}
        countLabel={`${(directoryMode ? rows.length : controller.listTotalCount ?? rows.length).toLocaleString()}${hasMore && directoryMode ? '+' : ''} ${facet === 'domains' ? (rows.length === 1 && !hasMore ? 'domain' : 'domains') : (rows.length === 1 && !hasMore ? 'person' : 'people')}`}
        error={directoryMode ? directory.error ?? directory.pageError : controller.listError} degraded={directoryMode ? null : controller.degraded}
        {facet} savedOnly={saved} {query} {activeKey} {selection} {filterSections} filterCount={chips.length} onResetFilters={reset} {chips} sort={directoryMode && sort === 'closest' ? 'name' : sort}
        sortOptions={[...(!directoryMode ? [{ value: 'closest', label: 'Closest' }] : []), { value: 'recent', label: 'Most recent' }, { value: 'name', label: 'Name' }, ...(directoryMode || sort === 'last_contact_asc' ? [{ value: 'last_contact_asc', label: 'Least recently contacted' }] : [])]}
        canDelete={selectedSaved.length > 0 && [...selection.explicitKeys].every(key => key.startsWith('person:'))} onDelete={() => confirmDelete = true}
        canMerge={selection.count >= 2 && [...selection.explicitKeys].every((key) => key.startsWith('cluster:'))}
        onFacetChange={(value) => onNavigate({ relationshipFacet: value, directoryPersonID: null, relationshipTarget: null, relationshipFiles: false })} onSortChange={(value) => onNavigate({ relationshipSort: value as ExploreURLState['relationshipSort'] })} onOpen={(key) => void open(key)}
        onLoadMore={loadMore} onRetry={() => directoryMode ? void directory.reloadFirstPage() : void controller.loadList(page.predicate)} onMerge={() => void startBulk()} />
    </div>
    {#if bulkMessage}<p role="status">{bulkMessage}</p>{/if}
    {#if staleLink}<Button label="Retry identity cache" onclick={() => void retryLink()} />{/if}
  </div>
{/snippet}
{#snippet person()}
  {#if soloPerson !== null && directory.selectedPersonID !== soloPerson}
    <section class="solo"><p role="status">Loading person…</p><Button label="Back to People" onclick={close} /></section>
  {:else}
  <PersonPage {...page} {directory} bind:details bind:selectedDate bind:domainYear bind:sort={fileSort} bind:filenameQuery bind:mimeFamilies
    onRecordDeleted={(deletedID) => void peopleDeleted([deletedID])}
    onSplitCommitted={async (context) => { onDuplicatesChanged(); onReplaceNavigation({ directoryPersonID: context.sourcePersonID }); await directory.reconcilePersonSplit(context); await controller.reconcilePersonMerge(controller.personMergeContextSnapshot()); }}
    onReconcilePersonMerge={(context, participantsLinked, success) => participantsLinked ? refreshProfiles() : reconcilePersonMerge(context, success)}
    onIdentityChange={() => { onDuplicatesChanged(); return directory.reloadFirstPage(); }} durablePersonID={soloPerson} onBack={narrow ? close : undefined} />
  {/if}
{/snippet}
<!-- svelte-ignore a11y_no_noninteractive_element_interactions -- Escape returns from a person to the list. -->
<main class="people" aria-label="People" bind:this={root} onkeydown={escape}>
  <!-- The heading repeats the tab at phone width; toolbar sizing follows touch. -->
  <div class="heading" class:heading--people={!urlState.peopleDuplicates}><PageHeader title={urlState.peopleDuplicates ? 'Possible duplicates' : 'People'}>{#snippet actions()}{#if urlState.peopleDuplicates}<Button label="Back to People" onclick={() => onNavigate({ peopleDuplicates: false })} />{/if}{/snippet}</PageHeader></div>
  {#if urlState.peopleDuplicates}
    <PossibleDuplicates controller={reviews} onDecide={(candidate, decision) => reviews.queueDecision(candidate, decision)} />
  {:else}
  <div class="body">
    {#if narrow}{#if hasPerson}<div class="solo-page" tabindex="-1">{@render person()}</div>{:else}{@render list()}{/if}
    {:else}<SplitPane ariaLabel="Resize people list" storageKey="msgvault.people.list-pane.size" initialSize={380} minPrimary={300} maxPrimary={480}>
      {#snippet primary()}{@render list()}{/snippet}{#snippet secondary()}{@render person()}{/snippet}
    </SplitPane>{/if}
  </div>
  {/if}
  {#if bulkConflict}<PersonBindingConflictModal client={page.client} conflict={bulkConflict} onOpenProfile={(id) => page.onOpenDirectoryPerson?.(id)} onSuccess={merged} onClose={() => { bulkConflict = undefined; bulkMessage = `${bulkCompleted} of ${bulkIDs.length - 1} linked. Linking stopped.`; }} />{/if}
  {#if confirmDelete}<Modal title="Delete people" onclose={() => { if (!bulkPending) confirmDelete = false; }}><p>This permanently deletes {selectedSaved.length} saved profiles, their contact points, and profile history. Archived messages stay.</p>{#snippet footer()}<Button label="Cancel" disabled={bulkPending} onclick={() => confirmDelete = false} /><Button label={bulkPending ? 'Deleting…' : 'Confirm delete'} disabled={bulkPending} tone="danger" onclick={() => void deleteSelected()} />{/snippet}</Modal>{/if}
</main>
<style>
  .people { display: flex; min-height: 0; height: 100%; flex-direction: column; background: var(--bg-canvas); }
  .heading { --page-header-gap: 0px; padding: var(--space-5) var(--page-gutter) var(--field-gap); }
  @media (max-width: 640px) {
    .heading--people { position: absolute; width: 1px; height: 1px; margin: -1px; padding: 0; overflow: hidden; clip-path: inset(50%); white-space: nowrap; border: 0; }
  }
  .body { display: flex; min-height: 0; flex: 1; }
  .list { display: flex; min-height: 0; height: 100%; width: 100%; flex-direction: column; }
  .list-content { display: flex; min-height: 0; flex: 1; }
  .filters { display: flex; flex-wrap: wrap; gap: var(--control-gap); padding: var(--space-3); }
  .filters label { display: flex; flex-direction: column; color: var(--text-secondary); font-size: var(--font-size-sm); }
  .solo-page { height: 100%; min-width: 0; display: flex; flex-direction: column; min-height: 0; flex: 1; }
  .solo { padding: var(--page-gutter); }
</style>
