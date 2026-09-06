<script lang="ts">
  import { Button, DetailDrawer, SearchInput, SelectDropdown, TextInput } from '@kenn-io/kit-ui';
  import { onDestroy, onMount, tick, untrack } from 'svelte';

  import type { APIClient } from '../../api/client';
  import type { DirectoryURLState } from '../../directory/models';
  import { DirectoryController } from '../../directory/controller.svelte';
  import { bufferedCallback } from '../../util/buffered-callback';
  import DirectoryList from './DirectoryList.svelte';
  import PersonDetail from './PersonDetail.svelte';

  interface Props {
    client: APIClient;
    controller: DirectoryController;
    state: DirectoryURLState;
    /** A caller may offer promotion only with an actual participant/cluster context. */
    promotionParticipantID?: number;
    onOpenCardDAVConflict?: (conflictID: number) => void;
    onOpenCardDAVSettings?: () => void;
    onAnnounce?: (message: string) => void;
  }

  let {
    client,
    controller,
    state: urlState,
    promotionParticipantID = undefined,
    onOpenCardDAVConflict = () => undefined,
    onOpenCardDAVSettings = () => undefined,
    onAnnounce = () => undefined
  }: Props = $props();
  let root = $state<HTMLElement>();
  let narrow = $state(false);
  let mediaQuery: MediaQueryList | undefined;

  const TEXT_FILTER_DEBOUNCE_MS = 250;
  type TextFilterKey = 'directoryQuery' | 'directoryCategory' | 'directoryOrganization' | 'directoryLastContactAfter' | 'directoryLastContactBefore';
  // Mirrors the controller's text filters for immediate display: the debounce
  // below only delays the controller write (and so the page-one fetch and the
  // URL replace it drives), never the text shown in the inputs. This matches
  // RelationshipsWorkspace's "local state for display, debounced write for the
  // network call" split.
  let textFilters = $state<Record<TextFilterKey, string>>(untrack(() => controllerTextFilters()));

  function controllerTextFilters(): Record<TextFilterKey, string> {
    return {
      directoryQuery: controller.query,
      directoryCategory: controller.category,
      directoryOrganization: controller.organization,
      directoryLastContactAfter: controller.lastContactAfter,
      directoryLastContactBefore: controller.lastContactBefore
    };
  }

  // The controller only changes these through its own commit or a URL
  // restoration (Back/Forward), so resyncing on every change never clobbers
  // text the user is still typing.
  $effect(() => { textFilters = controllerTextFilters(); });

  // Every pending edit is sent as one snapshot so typing in two inputs within
  // the debounce window loses neither. Text edits replace the current history
  // entry instead of pushing one, so Back never walks through partial queries.
  const debouncedTextFilters = bufferedCallback((patch: Record<TextFilterKey, string>) => {
    controller.setFilters(patch, 'replace');
  }, TEXT_FILTER_DEBOUNCE_MS);

  // Flush, not cancel: the controller (owned by AppShell) outlives this
  // component across a workspace round-trip, so the typed text is kept.
  onDestroy(() => debouncedTextFilters.flush());

  function editTextFilter(key: TextFilterKey, value: string): void {
    textFilters[key] = value;
    debouncedTextFilters({ ...textFilters });
  }

  // Selects apply immediately; a pending text edit lands first so the select
  // commit and its fetch already include what was typed.
  function selectFilter(patch: Partial<Omit<DirectoryURLState, 'directoryPersonID'>>): void {
    debouncedTextFilters.flush();
    controller.setFilters(patch);
  }

  const contactStateOptions = [
    { value: '', label: 'All contact states' },
    { value: 'active', label: 'Active' },
    { value: 'inactive', label: 'Inactive' }
  ];
  const primaryChannelOptions = [{ value: '', label: 'All channels' }, ...['email', 'phone', 'chat'].map((value) => ({ value, label: value }))];
  const sortOptions = [
    { value: 'name', label: 'Name' },
    { value: 'last_contact_desc', label: 'Most recently contacted' },
    { value: 'last_contact_asc', label: 'Least recently contacted' }
  ];

  $effect(() => {
    // Read every URL field outside untrack so AppShell history restoration
    // rehydrates this controller, but keep controller internals untracked:
    // selecting a row must never be mistaken for a URL change and cleared.
    const snapshot: DirectoryURLState = { ...urlState };
    untrack(() => controller.applyURLState(snapshot));
  });

  onMount(() => {
    if (!window.matchMedia) return;
    mediaQuery = window.matchMedia('(max-width: 760px)');
    const update = () => { narrow = mediaQuery?.matches ?? false; };
    update();
    mediaQuery.addEventListener('change', update);
    return () => mediaQuery?.removeEventListener('change', update);
  });

  async function closeDetail(): Promise<void> {
    await controller.selectPerson(null);
    await tick();
    // DetailDrawer's focus trap restores its previous element during
    // teardown. Yield one task so that cleanup finishes before placing focus
    // on Directory's roving row.
    await new Promise<void>((resolve) => setTimeout(resolve, 0));
    root?.querySelector<HTMLElement>('[role="row"][tabindex="0"]')?.focus();
  }

  async function promote(): Promise<void> {
    if (promotionParticipantID === undefined) return;
    await controller.promote(promotionParticipantID);
  }
</script>

<main class="directory-workspace" bind:this={root} aria-label="Directory">
  <header class="directory-toolbar">
    <div><h1>Directory</h1><p>Durable people and their recorded contact context.</p></div>
    {#if promotionParticipantID !== undefined}
      <Button label="Promote to person" tone="workflow" onclick={() => void promote()} />
    {/if}
  </header>
  <div class="filters">
    <SearchInput value={textFilters.directoryQuery} ariaLabel="Search directory" placeholder="Search people, email, or organization…" block oninput={(value) => editTextFilter('directoryQuery', value)} />
    <SelectDropdown title="Contact state" value={controller.contactState} options={contactStateOptions}
      onchange={(value) => selectFilter({ directoryContactState: value })} />
    <TextInput value={textFilters.directoryCategory} ariaLabel="Category filter" placeholder="Category"
      oninput={(value) => editTextFilter('directoryCategory', value)} />
    <TextInput value={textFilters.directoryOrganization} ariaLabel="Organization filter" placeholder="Organization"
      oninput={(value) => editTextFilter('directoryOrganization', value)} />
    <SelectDropdown title="Primary channel" value={controller.primaryChannel} options={primaryChannelOptions}
      onchange={(value) => selectFilter({ directoryPrimaryChannel: value })} />
    <TextInput value={textFilters.directoryLastContactAfter} ariaLabel="Last contacted after" placeholder="Contacted after (YYYY-MM-DD)"
      oninput={(value) => editTextFilter('directoryLastContactAfter', value)} />
    <TextInput value={textFilters.directoryLastContactBefore} ariaLabel="Last contacted before" placeholder="Contacted before (YYYY-MM-DD)"
      oninput={(value) => editTextFilter('directoryLastContactBefore', value)} />
    <SelectDropdown title="Directory order" value={controller.sort} options={sortOptions}
      onchange={(value) => selectFilter({ directorySort: value as DirectoryURLState['directorySort'] })} />
  </div>
  {#if controller.promotionResult && !controller.promotionResult.ok}
    <div role="alert" class="promotion-error">
      {controller.promotionResult.message}
      {#if controller.promotionResult.code === 'person_binding_conflict'} This participant already belongs to another durable person; resolve that binding before promoting it.{/if}
    </div>
  {/if}
  <div class="directory-content" class:has-detail={controller.selectedPersonID !== null && !narrow}>
    <DirectoryList
      rows={controller.rows}
      loading={controller.loading}
      loadingMore={controller.loadingMore}
      error={controller.error}
      pageError={controller.pageError}
      pageRecovery={controller.pageRecovery}
      hasMore={controller.cursor !== null}
      selectedPersonID={controller.selectedPersonID}
      onSelect={(personID) => void controller.selectPerson(personID)}
      onLoadMore={() => void controller.loadNextPage()}
      onReload={() => void controller.reloadFirstPage()}
    />
    {#if controller.selectedPersonID !== null && !narrow}
      <aside class="detail-pane" aria-label="Person detail">
        {#if controller.detailLoading}<p role="status">Loading person detail…</p>{:else if controller.detail}<PersonDetail {client} bundle={controller.detail} personID={controller.selectedPersonID} profileController={controller.profile} entityController={controller.entity} onOpenPerson={(personID) => void controller.selectPerson(personID)} onSplitCommitted={(context) => controller.reconcilePersonSplit(context)} {onOpenCardDAVConflict} {onOpenCardDAVSettings} {onAnnounce} />{/if}
      </aside>
    {/if}
  </div>
  {#if controller.selectedPersonID !== null && narrow}
    <DetailDrawer title="Person detail" ariaLabel="Person detail" onclose={() => void closeDetail()}>
      {#if controller.detailLoading}<p role="status">Loading person detail…</p>{:else if controller.detail}<PersonDetail {client} bundle={controller.detail} personID={controller.selectedPersonID} profileController={controller.profile} entityController={controller.entity} onOpenPerson={(personID) => void controller.selectPerson(personID)} onSplitCommitted={(context) => controller.reconcilePersonSplit(context)} {onOpenCardDAVConflict} {onOpenCardDAVSettings} {onAnnounce} />{/if}
    </DetailDrawer>
  {/if}
</main>

<style>
  .directory-workspace { padding: var(--space-5); display: grid; gap: var(--space-4); min-height: 0; }
  .directory-toolbar, .filters { display: flex; gap: var(--space-3); align-items: center; justify-content: space-between; flex-wrap: wrap; }
  h1, p { margin: 0; } .directory-toolbar p { color: var(--text-muted); font-size: var(--font-size-sm); }
  .filters { justify-content: stretch; } .filters :global(.kit-search-input) { min-width: min(100%, 300px); flex: 1; }
  .directory-content { display: grid; min-height: 0; } .directory-content.has-detail { grid-template-columns: minmax(260px, 0.8fr) minmax(360px, 1.2fr); gap: var(--space-4); }
  .detail-pane { border-left: 1px solid var(--border-default); min-width: 0; }
  .promotion-error { padding: var(--space-3); background: var(--bg-inset); color: var(--text-secondary); }
  @media (max-width: 760px) { .directory-workspace { padding: var(--space-3); } }
</style>
