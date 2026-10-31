<script lang="ts">
  import { appShortcuts, ROOT_SCOPE, Button, TextInput, SelectDropdown, type FilterDropdownSection } from '@kenn-io/kit-ui';
  import { onDestroy, onMount, tick, untrack } from 'svelte';
  import type { RelationshipsController, RelationshipsMergeContext } from '../../relationships/controller.svelte';
  import type { DirectoryController } from '../../directory/controller.svelte';
  import { PRIMARY_CHANNELS, channelLabel, contactStateLabel, formatDay } from '../../directory/labels';
  import { getPersonProfile } from '../../api/generated/api/api';
  import type { RelationshipRow } from '../../relationships/models';
  import type { DomainSummary, PersonSummary, ExploreURLState } from '../../explore/models';
  import { ExploreSelectionState } from '../../explore/state.svelte';
  import type { ValidatedPersonMergeRequired, PersonMergeSuccess } from '../../directory/person-merge';
  import PersonBindingConflictModal from '../directory/PersonBindingConflictModal.svelte';
  import SplitPane from '../layout/SplitPane.svelte';
  import PageHeader from '../shell/PageHeader.svelte';
  import PeopleList, { type PeopleRow } from './PeopleList.svelte';
  import PersonPage from './PersonPage.svelte';
  import type { ComponentProps } from 'svelte';

  type PageProps = Omit<ComponentProps<typeof PersonPage>, 'onBack' | 'onReconcilePersonMerge'>;
  let { directory, urlState, onNavigate, onReplaceNavigation, onDraftNavigation, onShowAllChange, ...page }:
    PageProps & { directory: DirectoryController; urlState: ExploreURLState; onReplaceNavigation: (patch: Partial<ExploreURLState>) => void;
      onNavigate: (patch: Partial<ExploreURLState>) => void; onDraftNavigation: (patch: Partial<ExploreURLState>) => void; onShowAllChange: (value: boolean) => void } = $props();
  const query = $derived(urlState.identityQuery ?? '');
  const controller = $derived(page.controller);
  const facet = $derived(urlState.relationshipFacet);
  const showAll = $derived(urlState.relationshipShowAll);
  const selection = new ExploreSelectionState();
  let root = $state<HTMLElement>();
  let narrow = $state(false);
  const saved = $derived(urlState.relationshipSaved);
  let filterOpen = $state(false);
  const sort = $derived(urlState.relationshipSort);
  let opening = $state(false);
  let openingError = $state('');
  let bulkPending = $state(false);
  let bulkMessage = $state<string | null>(null);
  let bulkConflict = $state<ValidatedPersonMergeRequired>();
  let bulkIDs = $state<number[]>([]);
  let bulkCompleted = $state(0);
  let bulkContext = $state<ReturnType<RelationshipsController['personMergeContextSnapshot']>>();
  let staleLink = $state<{ a: number; b: number } | null>(null);
  type SoloProfile = { id: number; display_name: string; targets: string[] };
  let soloProfile = $state<SoloProfile | null>(null);
  const profileFilters = $derived(Boolean(urlState.directoryContactState || urlState.directoryCategory || urlState.directoryOrganization || urlState.directoryPrimaryChannel || urlState.directoryLastContactAfter || urlState.directoryLastContactBefore));
  const directoryMode = $derived(facet === 'people' && (saved || profileFilters));
  const rankedList = $derived(facet === 'people' && !directoryMode && sort === 'closest' && query.trim() === '');
  const soloPerson = $derived(urlState.directoryPersonID);
  const predicateKey = $derived(JSON.stringify(page.predicate));
  $effect(() => {
    controller.facet = facet; controller.showAll = showAll; controller.query = query;
    controller.sort = sort === 'name' ? { field: 'display_label', direction: 'asc' } : sort === 'recent' ? { field: 'latest_at', direction: 'desc' } : { field: 'activity_count', direction: 'desc' };
    void predicateKey;
    if (!directoryMode) untrack(() => void controller.loadList(page.predicate));
  });
  $effect(() => {
    if (!directoryMode) return;
    directory.applyURLState({ ...urlState, directoryPersonID: null, directoryQuery: query, directorySort: sort === 'recent' ? 'last_contact_desc' : 'name' });
  });
  $effect(() => { void [facet, query, sort, directoryMode, predicateKey, showAll, urlState.directoryContactState, urlState.directoryCategory, urlState.directoryOrganization, urlState.directoryPrimaryChannel, urlState.directoryLastContactAfter, urlState.directoryLastContactBefore]; untrack(() => selection.clear()); });
  onMount(() => {
    const observer = new ResizeObserver(([entry]) => narrow = (entry?.contentRect.width ?? 1200) < 720);
    if (root) observer.observe(root);
    return () => observer.disconnect();
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
      return { key: `domain:${domain.domain}`, label: domain.domain, detail: `${domain.activity_count.toLocaleString()} messages`, lastAt: domain.last_at, saved: false, domain: true };
    }
    const person = row as PersonSummary;
    return { key: `cluster:${person.id}`, label: person.display_label, detail: `${person.activity_count.toLocaleString()} messages`, lastAt: person.last_at, saved: Boolean(person.profile), domain: false };
  }));
  const loading = $derived(directoryMode ? directory.loading : controller.listLoading);
  const hasMore = $derived(Boolean(directoryMode ? directory.cursor : controller.listCursor));
  const activeKey = $derived(soloPerson !== null ? `person:${soloPerson}` : page.target);
  const hasPerson = $derived(page.target !== null || soloPerson !== null);
  let openGeneration = 0;
  const navigationKey = $derived(JSON.stringify(urlState));
  $effect(() => { void navigationKey; untrack(() => { openGeneration += 1; opening = false; }); });
  onDestroy(() => { openGeneration += 1; });
  $effect(() => {
    void navigationKey;
    const id = soloPerson;
    if (id === null || showsProfile(soloProfile, id, page.target)) return;
    const abort = new AbortController();
    const generation = ++openGeneration;
    opening = page.target === null; openingError = '';
    void getPersonProfile({ id }, { ...page.client, signal: abort.signal }).then((response) => {
      if (abort.signal.aborted || generation !== openGeneration) return;
      if (!response.data) throw new Error('Saved person not found.');
      soloProfile = soloProfileOf(id, response.data);
      if (!showsProfile(soloProfile, id, page.target)) onReplaceNavigation({ relationshipTarget: soloProfile.targets[0] ?? null, relationshipFiles: false });
    }).catch((cause: unknown) => { if (!abort.signal.aborted && generation === openGeneration) openingError = cause instanceof Error ? cause.message : 'Could not open this person.'; })
      .finally(() => { if (generation === openGeneration) opening = false; });
    return () => abort.abort();
  });
  function soloProfileOf(id: number, profile: { display_name?: string; participant_ids?: number[] }): SoloProfile {
    return { id, display_name: profile.display_name || 'Saved person', targets: (profile.participant_ids ?? []).map((participant) => `cluster:${participant}`) };
  }
  function showsProfile(profile: SoloProfile | null, id: number, target: string | null): boolean {
    return profile?.id === id && (profile.targets.length ? profile.targets.includes(target ?? '') : target === null);
  }
  async function open(key: string): Promise<void> {
    const generation = ++openGeneration;
    openingError = '';
    if (!key.startsWith('person:')) { opening = false; onNavigate({ directoryPersonID: null, relationshipTarget: key, relationshipFiles: false }); return; }
    const id = Number(key.slice(7));
    opening = true;
    try {
      const response = await getPersonProfile({ id }, page.client);
      if (generation !== openGeneration) return;
      if (!response.data) throw new Error('Could not open this person.');
      soloProfile = soloProfileOf(id, response.data);
      onNavigate({ directoryPersonID: id, relationshipTarget: soloProfile.targets[0] ?? null, relationshipFiles: false });
    } catch (cause: unknown) { if (generation === openGeneration) openingError = cause instanceof Error ? cause.message : 'Could not open this person.'; }
    finally { if (generation === openGeneration) opening = false; }
  }
  function escape(event: KeyboardEvent): void {
    if (event.key !== 'Escape' || !hasPerson || appShortcuts.activeScope() !== ROOT_SCOPE) return;
    if ((event.target as Element | null)?.closest('input, textarea, select, [contenteditable]') || document.querySelector('.kit-popover-card')) return;
    close();
    event.preventDefault();
    event.stopPropagation();
    void tick().then(() => root?.querySelector<HTMLElement>('[aria-label="People results"]')?.focus());
  }
  function close(): void { openGeneration += 1; opening = false; onNavigate({ directoryPersonID: null, relationshipTarget: null, relationshipFiles: false }); }
  function reset(): void { onNavigate({ relationshipSaved: false, relationshipShowAll: false, directoryContactState: '', directoryCategory: '', directoryOrganization: '', directoryPrimaryChannel: '', directoryLastContactAfter: '', directoryLastContactBefore: '' }); }
  const filterSections = $derived<FilterDropdownSection[]>([{ items: [
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
  async function startBulk(): Promise<void> {
    const ids = [...selection.explicitKeys].map((key) => /^cluster:([1-9][0-9]*)$/.exec(key)?.[1]).filter((id): id is string => id !== undefined).map(Number);
    if (ids.length < 2 || bulkPending || bulkConflict) return;
    bulkIDs = ids; bulkCompleted = 0; bulkContext = controller.personMergeContextSnapshot();
    await continueBulk();
  }
  async function continueBulk(): Promise<void> {
    if (bulkIDs.length < 2) return;
    bulkPending = true;
    while (bulkCompleted < bulkIDs.length - 1) {
      const next = bulkIDs[bulkCompleted + 1]!;
      const result = await controller.linkParticipants(bulkIDs[0]!, next);
      if (result.ok || result.code === 'already_linked') {
        if (result.ok) staleLink = result.cacheState === 'stale' ? { a: bulkIDs[0]!, b: next } : null;
        bulkCompleted += 1; continue;
      }
      bulkPending = false;
      if (result.code === 'merge_required') { bulkConflict = result.conflict; return; }
      bulkMessage = `${bulkCompleted} of ${bulkIDs.length - 1} linked. ${result.message}`; return;
    }
    bulkPending = false; selection.clear();
    bulkMessage = staleLink ? 'People linked. The identity cache needs a retry.' : 'People are now treated as the same person.';
  }
  async function reconcilePersonMerge(context: RelationshipsMergeContext): Promise<void> {
    await Promise.all([controller.reconcilePersonMerge(context), directory.reloadFirstPage()]);
  }
  async function merged(success: PersonMergeSuccess): Promise<void> {
    bulkPending = true;
    bulkConflict = undefined;
    page.onAnnounce?.(`Profiles merged into ${success.survivor.display_name}.`);
    if (bulkContext) await reconcilePersonMerge(bulkContext);
    await continueBulk();
  }
  async function retryLink(): Promise<void> {
    if (!staleLink) return;
    const result = await controller.linkParticipants(staleLink.a, staleLink.b);
    if (result.ok && result.cacheState === 'ready') { staleLink = null; bulkMessage = 'Identity cache refreshed.'; }
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
        {facet} {query} {activeKey} {selection} {filterSections} filterCount={chips.length} onResetFilters={reset} {chips} sort={directoryMode && sort === 'closest' ? 'name' : sort}
        sortOptions={[...(!directoryMode ? [{ value: 'closest', label: 'Closest' }] : []), { value: 'recent', label: 'Most recent' }, { value: 'name', label: 'Name' }]}
        canMerge={selection.count >= 2 && [...selection.explicitKeys].every((key) => key.startsWith('cluster:'))}
        onFacetChange={(value) => onNavigate({ relationshipFacet: value, directoryPersonID: null, relationshipTarget: null, relationshipFiles: false })} onSortChange={(value) => onNavigate({ relationshipSort: value as ExploreURLState['relationshipSort'] })} onOpen={(key) => void open(key)}
        onLoadMore={loadMore} onRetry={() => directoryMode ? void directory.reloadFirstPage() : void controller.loadList(page.predicate)} onMerge={() => void startBulk()} />
    </div>
    {#if bulkMessage}<p role="status">{bulkMessage}</p>{/if}
    {#if staleLink}<Button label="Retry identity cache" onclick={() => void retryLink()} />{/if}
  </div>
{/snippet}
{#snippet person()}
  {#if opening}<section class="solo"><p role="status">Loading person…</p><Button label="Back to People" onclick={close} /></section>
  {:else if soloPerson !== null && (page.target === null || controller.detailMissing)}
    {#if soloProfile?.id === soloPerson}<div class="solo-page"><section class="solo"><Button label="Back to People" onclick={close} /><h2>{soloProfile.display_name}</h2><Button label="Open in Directory" onclick={() => page.onOpenDirectoryPerson?.(soloPerson!)} /></section>{#if page.target !== null}<PersonPage {...page} onReconcilePersonMerge={reconcilePersonMerge} durablePersonID={soloPerson} />{/if}</div>{:else if openingError}<section class="solo"><Button label="Back to People" onclick={close} /><Button label="Retry" onclick={() => void open(`person:${soloPerson}`)} /></section>{/if}
  {:else}<PersonPage {...page} onReconcilePersonMerge={reconcilePersonMerge} durablePersonID={soloPerson} onBack={narrow ? close : undefined} />{/if}
{/snippet}
<!-- svelte-ignore a11y_no_noninteractive_element_interactions -- Escape returns from a person to the list. -->
<main class="people" aria-label="People" bind:this={root} onkeydown={escape}>
  <div class="heading"><PageHeader title="People">{#snippet actions()}<Button label="Directory" onclick={() => onNavigate({ workspace: 'directory' })} /><Button label="Reviews" onclick={() => onNavigate({ workspace: 'directory_review' })} />{/snippet}</PageHeader></div>
  {#if openingError}<p role="alert">{openingError}</p>{/if}
  <div class="body">
    {#if narrow}{#if hasPerson}{@render person()}{:else}{@render list()}{/if}
    {:else}<SplitPane ariaLabel="Resize people list" storageKey="msgvault.people.list-pane.size" initialSize={380} minPrimary={300} maxPrimary={480}>
      {#snippet primary()}{@render list()}{/snippet}{#snippet secondary()}{@render person()}{/snippet}
    </SplitPane>{/if}
  </div>
  {#if bulkConflict}<PersonBindingConflictModal client={page.client} conflict={bulkConflict} onOpenProfile={(id) => page.onOpenDirectoryPerson?.(id)} onSuccess={merged} onClose={() => { bulkConflict = undefined; bulkMessage = `${bulkCompleted} of ${bulkIDs.length - 1} linked. Linking stopped.`; }} />{/if}
</main>
<style>
  .people { display: flex; min-height: 0; height: 100%; flex-direction: column; background: var(--bg-canvas); }
  .heading { padding: var(--space-5) var(--page-gutter) var(--field-gap); }
  .body { display: flex; min-height: 0; flex: 1; }
  .list { display: flex; min-height: 0; height: 100%; width: 100%; flex-direction: column; }
  .list-content { display: flex; min-height: 0; flex: 1; }
  .filters { display: flex; flex-wrap: wrap; gap: var(--control-gap); padding: var(--space-3); }
  .filters label { display: flex; flex-direction: column; color: var(--text-secondary); font-size: var(--font-size-sm); }
  .solo-page { height: 100%; min-width: 0; display: flex; flex-direction: column; min-height: 0; flex: 1; }
  .solo { padding: var(--page-gutter); }
</style>
