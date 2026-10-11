<script module lang="ts">
  export type HubLayout = 'wide' | 'narrow';

  /** Pure container-width → layout classification, kept outside the
   * component so it's unit-testable without a real ResizeObserver (jsdom
   * doesn't fire one). Below 720px the list becomes a slide-in drawer; the
   * reading pane always stacks under the timeline, so no intermediate
   * breakpoint remains. */
  export function computeHubLayout(width: number): HubLayout {
    return width < 720 ? 'narrow' : 'wide';
  }
</script>

<script lang="ts">
  import { appShortcuts, Button, DetailDrawer, EmptyState, ROOT_SCOPE } from '@kenn-io/kit-ui';
  import { onDestroy, onMount, tick, untrack } from 'svelte';

  import type { MeetingRef } from '../../api/generated/models';
  import MeetingPanel from '../meetings/MeetingPanel.svelte';
  import { relationshipMeetingScope } from '../../meetings/scopes';
  import type { APIClient } from '../../api/client';
  import { listPersonAttributes } from '../../api/generated/api/api';
  import type { ExplorePredicate, FileMIMEFamily, FileSearchSort, PersonFileDirection } from '../../explore/models';
  import { ExploreSelectionState } from '../../explore/state.svelte';
  import type { RelationshipsController } from '../../relationships/controller.svelte';
  import type { RelationshipFacet, RelationshipTimelineRow } from '../../relationships/models';
  import type { PersonMergeSuccess, ValidatedPersonMergeRequired } from '../../directory/person-merge';
  import { bufferedCallback } from '../../util/buffered-callback';
  import FilesWorkspace from '../files/FilesWorkspace.svelte';
  import SplitPane from '../layout/SplitPane.svelte';
  import PersonBindingConflictModal from '../directory/PersonBindingConflictModal.svelte';
  import ReadingPane, { type ReadingPaneSelection } from '../reader/ReadingPane.svelte';
  import RelationshipHeader from './RelationshipHeader.svelte';
  import RelationshipCalendar from './RelationshipCalendar.svelte';
  import RelationshipList from './RelationshipList.svelte';
  import PageHeader from '../shell/PageHeader.svelte';
  import RelationshipTimeline from './RelationshipTimeline.svelte';
  import { localDayBoundsUTC, timelineRowToSelection } from './timeline-support';

  const QUERY_DEBOUNCE_MS = 250;

  interface Props {
    client: APIClient;
    controller: RelationshipsController;
    facet: RelationshipFacet;
    target: string | null;
    showAll: boolean;
    filesOpen: boolean;
    predicate: ExplorePredicate;
    personFilePresentation?: 'media' | 'files';
    personFileDirections?: PersonFileDirection[];
    onFacetChange: (facet: RelationshipFacet) => void;
    onTargetChange: (target: string | null) => void;
    onShowAllChange: (value: boolean) => void;
    onFilesToggle: (value: boolean) => void;
    onPersonFilePresentationChange?: (value: 'media' | 'files') => void;
    onPersonFileDirectionsChange?: (value: PersonFileDirection[]) => void;
    /** Degraded-state escape hatch: switches the parent workspace to
     * Everything. Not part of the frozen Task 4 Props contract — AppShell
     * (Task 6) wires it to its own workspace-change callback. */
    onOpenEverything?: () => void;
    /** Opens Directory with the currently loaded, API-validated participant. */
    onOpenDirectory?: (participantID: number) => void;
    onOpenDirectoryPerson?: (personID: number) => void;
    onAnnounce?: (message: string) => void;
    onOpenMeeting?: (meeting: MeetingRef) => void;
    /** Opening a file (or its containing conversation) from the hub's own
     * embedded Files pane has no reading pane of its own to resolve a full
     * EntryRow into — AppShell wires these to the same openFileItem/
     * openFileConversation it uses for the Files sibling workspace, which
     * navigate to Everything with the item selected. Omitted callbacks
     * leave FileViewer's open buttons as a no-op, same as before this prop
     * existed. */
    onOpenFileItem?: (entryKey: string) => void;
    onOpenFileConversation?: (entryKey: string, messageID: number, conversationID: number) => void;
  }

  let {
    client,
    controller,
    facet,
    target,
    showAll,
    filesOpen,
    predicate,
    personFilePresentation = 'files',
    personFileDirections = ['from_person'],
    onFacetChange,
    onTargetChange,
    onShowAllChange,
    onFilesToggle,
    onPersonFilePresentationChange = undefined,
    onPersonFileDirectionsChange = undefined,
    onOpenEverything = undefined,
    onOpenDirectory = undefined,
    onOpenDirectoryPerson = undefined,
    onAnnounce = undefined,
    onOpenMeeting = undefined,
    onOpenFileItem = undefined,
    onOpenFileConversation = undefined
  }: Props = $props();

  let selection = $state<ReadingPaneSelection | undefined>();
  let conversationAnchorId = $state<number | undefined>();
  let conversationBounds = $state<{ start: string; end: string } | undefined>();
  let mobileListOpen = $state(false);
  let fileSort = $state<FileSearchSort>({ field: 'occurred_at', direction: 'desc' });
  let fileFilenameQuery = $state('');
  let fileMIMEFamilies = $state<FileMIMEFamily[]>([]);
  let rootElement = $state<HTMLElement>();
  let containerWidth = $state(1200);
  // Mirrors controller.query for immediate display: the debounce below only
  // delays *writing* controller.query (and so the search fetch it drives),
  // never the text shown in the search box, matching LinkIdentityDialog's
  // "local state for display, debounced write for the network call" split.
  let queryInput = $state(untrack(() => controller.query));
  const listSelection = new ExploreSelectionState();
  let bulkPending = $state(false);
  let bulkMessage = $state<string | null>(null);
  let bulkError = $state(false);
  let bulkConflict = $state<ValidatedPersonMergeRequired>();
  let bulkAnchorID = $state<number>();
  let bulkQueue = $state<number[]>([]);
  let bulkCompleted = $state(0);
  let bulkTotal = $state(0);
  let bulkContext = $state<ReturnType<RelationshipsController['personMergeContextSnapshot']>>();
  // The latest successful link reports whether the identity cache refreshed.
  // A stale cache keeps a warning and Retry, which repeats this idempotent link.
  let bulkStaleLink = $state<{ a: number; b: number } | null>(null);

  // A dedicated instance per component, never shared — LinkIdentityDialog's
  // own debounced search is a separate instance for the same reason; sharing
  // one across components previously caused a cross-component bug.
  const debouncedSetQuery = bufferedCallback((value: string) => { controller.query = value; }, QUERY_DEBOUNCE_MS);

  // Flush, not cancel: the controller (owned by AppShell) outlives this
  // component across a workspace round-trip, so a flushed write is not
  // lost — it just sits on controller.query until the hub remounts and its
  // own load effect below fires again on mount, refetching with the typed
  // text intact. Cancelling here would silently drop what the user typed.
  onDestroy(() => debouncedSetQuery.flush());

  function handleQueryChange(value: string): void {
    queryInput = value;
    if (!bulkPending && !bulkConflict) listSelection.clear();
    if (!bulkStaleLink) {
      bulkMessage = null;
      bulkError = false;
    }
    debouncedSetQuery(value);
  }

  // Stable selectors into panes that never unmount when the reading pane or
  // drawer closes, so focus always lands somewhere real instead of falling
  // to <body> (mirrors AppShell's currentGrid()/document.querySelector
  // pattern rather than needing bind:this on every target).
  const TIMELINE_GRID_SELECTOR = '[role="grid"][aria-label="Relationship activity"]';
  const FILES_GRID_SELECTOR = '[role="grid"][aria-label="Files results"]';
  const LIST_GRID_SELECTOR = '[role="grid"][aria-label="Relationship results"]';
  const layout = $derived(computeHubLayout(containerWidth));
  const predicateFingerprint = $derived(JSON.stringify(predicate));
  const selectedRowKey = $derived(selection?.kind === 'entry' ? selection.row.key : null);

  let previousSelectionContext = '';
  // A context change during a batch (Back/Forward can change filters while
  // the list is locked) must still drop the selection once the batch stops,
  // so the next batch cannot link people the new filters hide.
  let selectionResetPending = false;
  $effect(() => {
    const next = `${facet}\u0000${showAll}\u0000${controller.query}\u0000${predicateFingerprint}`;
    const locked = bulkPending || bulkConflict !== undefined;
    if (next !== previousSelectionContext) {
      previousSelectionContext = next;
      if (locked) {
        selectionResetPending = true;
        return;
      }
      untrack(() => listSelection.clear());
      if (!untrack(() => bulkStaleLink)) {
        bulkMessage = null;
        bulkError = false;
      }
      return;
    }
    if (!locked && selectionResetPending) {
      selectionResetPending = false;
      untrack(() => listSelection.clear());
    }
  });

  // One effect ties facet/showAll/query/predicate to a single loadList call
  // — mirrors PeopleWorkspace's search effect (the fingerprint sweep is
  // Task 7's job across the whole app, not a new convention to invent here).
  $effect(() => {
    controller.facet = facet;
    controller.showAll = showAll;
    const query = controller.query;
    void query;
    void predicateFingerprint;
    void controller.loadList(untrack(() => predicate));
  });

  onMount(() => {
    if (!rootElement) return;
    containerWidth = rootElement.clientWidth || containerWidth;
    if (typeof ResizeObserver === 'undefined') return;
    const observer = new ResizeObserver((entries) => {
      const measured = entries[0]?.contentRect.width;
      if (measured !== undefined) containerWidth = measured;
    });
    observer.observe(rootElement);
    return () => observer.disconnect();
  });

  // Mirrors the controller's contextPredicate: the hub applies no text query
  // on any surface (the relationships ranking/cluster-timeline endpoints have
  // no text-query input), so the files pane and reading pane must not apply
  // one either when a stale URL still carries it.
  function contextPredicate(value: ExplorePredicate): ExplorePredicate {
    const {
      cursor: _cursor, grouping: _grouping, candidate_snapshot_id: _snapshot,
      query: _query, search_mode: _searchMode, ...context
    } = value;
    return { ...context, presentation: 'table' };
  }

  function domainOf(value: string | null): string | undefined {
    return value?.startsWith('domain:') ? value.slice('domain:'.length) : undefined;
  }

  function identityScopeFor(value: string | null): { kind: 'person'; id: number } | { kind: 'domain'; domain: string } | undefined {
    const domain = domainOf(value);
    if (domain) return { kind: 'domain', domain };
    return controller.canonicalID !== null ? { kind: 'person', id: controller.canonicalID } : undefined;
  }

  // Domain targets resolve identityScopeFor synchronously (the domain name
  // comes straight from the target string), but a cluster target only knows
  // its canonicalID once the timeline response for openTarget lands. Until
  // then, identityScopeFor(target) is undefined — which FilesWorkspace reads
  // as "no identity scope, search the whole archive" (its correct meaning
  // for Everything/Files). Mounting FilesWorkspace in that gap would fire an
  // unscoped whole-archive search and flash the wrong rows, so the hub keeps
  // showing the timeline's own loading state until the scope resolves. A
  // null target (Esc/Back cleared it) always counts as files-closed too.
  // The controller.target !== target check covers a fast target switch: the
  // `target` prop can update to the new cluster before controller.openTarget
  // for it has run its synchronous reset, leaving controller.canonicalID
  // briefly holding the PREVIOUS cluster's id — without this check that
  // stale id would flow straight into identityScopeFor and FilesWorkspace
  // would mount scoped to the wrong person.
  const clusterScopePending = $derived(
    target !== null && domainOf(target) === undefined &&
    (controller.target !== target || controller.canonicalID === null)
  );
  const filesReady = $derived(target !== null && !clusterScopePending);
  const meetingContext = $derived.by(() => {
    if (!target || controller.target !== target || clusterScopePending) return undefined;
    const domain = domainOf(target);
    try {
      return { scope: relationshipMeetingScope(domain ? { domains: [domain] } : { participant_id: controller.canonicalID! }, contextPredicate(predicate)) };
    } catch (cause: unknown) {
      return { error: cause instanceof Error ? cause.message : 'Meeting activity cannot use these filters.' };
    }
  });

  function focusPane(selector: string): boolean {
    const element = rootElement?.querySelector<HTMLElement>(selector);
    if (!element) return false;
    element.focus();
    return true;
  }

  // The timeline/files grid never unmounts when the reading pane closes, so
  // this always has somewhere real to land focus — falls back to the files
  // grid when the center pane is showing FilesWorkspace instead.
  async function focusTimelinePane(): Promise<void> {
    await tick();
    if (!focusPane(TIMELINE_GRID_SELECTOR)) focusPane(FILES_GRID_SELECTOR);
  }

  // In narrow layout with the drawer closed, the list grid is inert (see
  // .pane-list's `inert` binding below) and cannot actually receive focus —
  // the browser silently refuses .focus() on it. Land on the drawer toggle
  // instead, the one focusable stand-in for "the list" in that state.
  async function focusListPane(): Promise<void> {
    await tick();
    if (layout === 'narrow' && !mobileListOpen) {
      rootElement?.querySelector<HTMLButtonElement>('.drawer-toggle')?.focus();
      return;
    }
    focusPane(LIST_GRID_SELECTOR);
  }

  // Closing the reading pane leaves the hub's own keydown handler as the
  // next-highest focus target (AppShell.closeReadingPane follows the same
  // shape): only re-focus the timeline when a pane was actually open, so
  // this stays a no-op when called defensively (e.g. from selectListRow).
  async function closeReadingPane(): Promise<void> {
    if (clearReadingPane()) await focusTimelinePane();
  }

  function clearReadingPane(): boolean {
    const wasOpen = selection !== undefined;
    selection = undefined;
    conversationAnchorId = undefined;
    conversationBounds = undefined;
    return wasOpen;
  }

  function selectListRow(nextTarget: string): void {
    void closeReadingPane();
    const wasDrawerOpen = mobileListOpen;
    mobileListOpen = false;
    onTargetChange(nextTarget);
    void controller.openTarget(nextTarget, predicate);
    // Closing the drawer makes it inert; if focus was inside it (e.g. the
    // search input auto-focused on open), the browser blurs it to <body>.
    // Re-home focus on the timeline so Esc-chaining keeps working.
    if (wasDrawerOpen) void focusTimelinePane();
  }

  function selectedPersonIDs(): number[] {
    return [...listSelection.explicitKeys]
      .map((target) => /^cluster:([1-9][0-9]*)$/.exec(target)?.[1])
      .filter((id): id is string => id !== undefined)
      .map(Number);
  }

  async function startBulkSamePerson(): Promise<void> {
    if (bulkPending || bulkConflict) return;
    const ids = selectedPersonIDs();
    if (ids.length < 2) return;
    bulkAnchorID = ids[0];
    bulkQueue = ids.slice(1);
    bulkCompleted = 0;
    bulkTotal = bulkQueue.length;
    bulkContext = controller.personMergeContextSnapshot();
    bulkError = false;
    bulkPending = true;
    await continueBulkSamePerson();
  }

  async function continueBulkSamePerson(): Promise<void> {
    if (bulkAnchorID === undefined) return;
    while (bulkQueue.length > 0) {
      const nextID = bulkQueue[0]!;
      bulkMessage = `Linking ${bulkCompleted + 1} of ${bulkTotal}…`;
      const outcome = await controller.linkParticipants(bulkAnchorID, nextID);
      if (outcome.ok || outcome.code === 'already_linked') {
        if (outcome.ok) bulkStaleLink = outcome.cacheState === 'stale' ? { a: bulkAnchorID, b: nextID } : null;
        bulkQueue = bulkQueue.slice(1);
        bulkCompleted += 1;
        continue;
      }
      if (outcome.code === 'merge_required') {
        bulkPending = false;
        bulkConflict = outcome.conflict;
        bulkMessage = 'Resolve this profile merge to continue linking the selection.';
        return;
      }
      bulkPending = false;
      bulkError = true;
      bulkMessage = `${bulkCompleted} of ${bulkTotal} linked. ${outcome.message}${staleCacheNote()}`;
      return;
    }
    bulkPending = false;
    bulkError = bulkStaleLink !== null;
    bulkMessage = `${bulkTotal + 1} people are now treated as the same person.${staleCacheNote()}`;
    listSelection.clear();
    onAnnounce?.(bulkMessage);
  }

  async function completeBulkMerge(success: PersonMergeSuccess): Promise<void> {
    const context = bulkContext;
    // Stay locked while the merge reconciles; otherwise Same person is enabled
    // again and a second batch would share this batch's queue and counters.
    bulkPending = true;
    bulkConflict = undefined;
    onAnnounce?.(`Profiles merged into ${success.survivor.display_name?.trim() || `Person ${success.survivor.id}`}.`);
    if (context) await controller.reconcilePersonMerge(context);
    await continueBulkSamePerson();
  }

  function cancelBulkMerge(): void {
    bulkConflict = undefined;
    bulkQueue = [];
    bulkError = true;
    bulkMessage = `${bulkCompleted} of ${bulkTotal} linked. Bulk linking was stopped.${staleCacheNote()}`;
  }

  function staleCacheNote(): string {
    return bulkStaleLink
      ? ' The cache refresh failed — groupings may be stale until a rebuild. Retrying is safe.'
      : '';
  }

  async function retryBulkCacheRefresh(): Promise<void> {
    if (!bulkStaleLink || bulkPending) return;
    const { a, b } = bulkStaleLink;
    bulkPending = true;
    try {
      const outcome = await controller.linkParticipants(a, b);
      if (!outcome.ok || outcome.cacheState === 'stale') return;
      bulkStaleLink = null;
      bulkError = false;
      bulkMessage = 'Identity cache refreshed.';
    } finally {
      bulkPending = false;
    }
  }

  // Esc closes the reading pane before it ever clears `target` (see
  // handleEscape below), and selectListRow closes it before switching, so an
  // in-component walk-back never reaches here with a pane still open. An
  // EXTERNAL change — browser Back/Forward, which rewrites `target` via the
  // URL without going through either path — skips that ordering, and can
  // jump straight between two non-null targets as well as clear to null.
  // Reset on ANY target change so the reading pane never keeps showing a
  // message from the cluster/domain that just closed underneath it. Plain
  // state reset only, no focus side effect: AppShell owns focus restoration
  // for history navigation already.
  let previousTarget: string | null = untrack(() => target);
  $effect(() => {
    if (target === previousTarget) return;
    previousTarget = target;
    clearReadingPane();
  });

  async function closeDrawer(): Promise<void> {
    mobileListOpen = false;
    await tick();
    rootElement?.querySelector<HTMLElement>('.drawer-toggle')?.focus();
  }

  // Keep the hub's raw Escape handler inactive while Kit owns the open
  // drawer's focus trap and dismissal.
  $effect(() => {
    if (layout !== 'narrow' || !mobileListOpen) return;
    return appShortcuts.pushScope('relationships-list-drawer');
  });

  // Every row opens its conversation thread directly in the reading pane.
  // chat_burst rows bound the window to the burst's local day; other rows
  // open the full window anchored at the row's own message.
  function openTimelineRow(row: RelationshipTimelineRow): void {
    selection = timelineRowToSelection(row);
    conversationAnchorId = row.anchor_message_id;
    conversationBounds = row.kind === 'chat_burst' && row.conversation_id !== undefined &&
      row.anchor_message_id !== undefined
      ? localDayBoundsUTC(row.first_at ?? row.occurred_at)
      : undefined;
  }

  // Focus stays on the calendar control the person just used.
  function selectCalendarDate(date: string | null): void {
    clearReadingPane();
    if (filesOpen) onFilesToggle(false);
    void controller.selectTimelineDay(date
      ? { date, ...localDayBoundsUTC(`${date}T00:00:00`) }
      : null);
  }

  // The day filters only the message timeline, so Files never shows a day.
  $effect(() => {
    if (!filesOpen || !controller.timelineDay) return;
    untrack(() => { void controller.selectTimelineDay(null); });
  });

  function editableTarget(value: EventTarget | null): boolean {
    const element = value as HTMLElement | null;
    return Boolean(element?.closest('input, textarea, select, [contenteditable]:not([contenteditable="false"])'));
  }

  // Esc walks back exactly one layer: reading pane → timeline → list →
  // nothing (bubbles further for the parent, AppShell in Task 6, to handle).
  // Bails out while a scope is pushed (e.g. LinkIdentityDialog, rendered
  // inside this same <main> and so a DOM descendant of it) so this raw
  // keydown handler doesn't stopPropagation the Escape before it reaches
  // the dialog's own Modal, which closes itself via a window-level
  // listener further up the bubble chain.
  function handleEscape(event: KeyboardEvent): void {
    if (event.key !== 'Escape' || editableTarget(event.target)) return;
    if (appShortcuts.activeScope() !== ROOT_SCOPE) return;
    if (selection !== undefined) {
      void closeReadingPane();
      event.preventDefault();
      event.stopPropagation();
      return;
    }
    if (target !== null) {
      onTargetChange(null);
      event.preventDefault();
      event.stopPropagation();
      void focusListPane();
    }
  }
</script>

{#snippet listPane()}
  <RelationshipList
    rows={controller.listRows}
    loading={controller.listLoading}
    loadingMore={controller.listLoadingMore}
    hasMore={Boolean(controller.listCursor)}
    totalCount={controller.listTotalCount}
    error={controller.listError}
    degraded={controller.degraded}
    {facet}
    query={queryInput}
    {showAll}
    autofocusSearch={layout === 'narrow' && mobileListOpen}
    activeTarget={target}
    selection={listSelection}
    {bulkPending}
    {bulkMessage}
    {bulkError}
    onQueryChange={handleQueryChange}
    {onFacetChange}
    {onShowAllChange}
    onSelect={selectListRow}
    onBulkSamePerson={() => { void startBulkSamePerson(); }}
    onBulkRetry={bulkStaleLink ? () => { void retryBulkCacheRefresh(); } : undefined}
    onLoadMore={() => { void controller.loadMoreList(); }}
    {onOpenEverything}
  />
{/snippet}

{#snippet centerAndReading()}
  <div class="pane-center-and-reading">
    <SplitPane
      ariaLabel="Resize reading pane"
      storageKey="msgvault.reading-pane.size"
      orientation="vertical"
      initialFraction={0.55}
      minPrimary={120}
      minSecondary={160}
      collapsed={selection === undefined}
    >
      {#snippet primary()}
        <div class="pane-center">
          {#if target === null && controller.target === null}
            <div class="hub-empty">
              <EmptyState
                title="Select a person or domain"
                description="Choose someone from the list to see your shared history across mail, chat, and files."
              />
            </div>
          {:else}
            <div class="pane-center-column">
              <div class="relationship-overview">
                <RelationshipHeader
                  detail={controller.detail}
                  loading={controller.timelineLoading}
                  {filesOpen}
                  {onFilesToggle}
                  {client}
                  {onOpenDirectory}
                  {onOpenDirectoryPerson}
                  loadAttributes={async (id) => (await listPersonAttributes({ id }, { history: false }, { ...client })).data?.attributes ?? []}
                  {onAnnounce}
                  capturePersonMergeContext={() => controller.personMergeContextSnapshot()}
                  onReconcilePersonMerge={(context) => controller.reconcilePersonMerge(context)}
                  onLinkParticipants={(a, b) => controller.linkParticipants(a, b)}
                  onUnlinkParticipants={(a, b) => controller.unlinkParticipants(a, b)}
                />
                {#if target !== null && domainOf(target) === undefined}
                  <RelationshipCalendar
                    calendar={controller.relationshipCalendar}
                    loading={controller.relationshipCalendarLoading}
                    error={controller.relationshipCalendarError}
                    year={controller.relationshipCalendarYear}
                    firstYear={controller.relationshipCalendarFirstYear}
                    currentYear={controller.relationshipCalendarCurrentYear}
                    selectedDate={controller.timelineDay?.date}
                    onDateChange={selectCalendarDate}
                    onYearChange={(year) => {
                      if (controller.timelineDay) selectCalendarDate(null);
                      void controller.loadRelationshipYear(year);
                    }}
                  />
                {/if}
                {#if meetingContext?.scope}
                  <details class="meeting-overview" open>
                    <summary>Meeting activity and follow-ups</summary>
                    <MeetingPanel {client} scope={meetingContext.scope} refreshKey={String(controller.identityRevision ?? '')} showHeading={false} {onOpenMeeting} />
                  </details>
                {:else if meetingContext?.error}
                  <p role="status">{meetingContext.error}</p>
                {/if}
              </div>
              <div class="relationship-activity">
                {#if filesOpen && filesReady}
                  <FilesWorkspace
                    {client}
                    embedded
                    predicate={contextPredicate(predicate)}
                    identityScope={identityScopeFor(target)}
                    sort={fileSort}
                    filenameQuery={fileFilenameQuery}
                    mimeFamilies={fileMIMEFamilies}
                    personPresentation={personFilePresentation}
                    personDirections={personFileDirections}
                    onSortChange={(value) => (fileSort = value)}
                    onFilenameQueryChange={(value) => (fileFilenameQuery = value)}
                    onMIMEFamiliesChange={(value) => (fileMIMEFamilies = value)}
                    onPersonPresentationChange={onPersonFilePresentationChange}
                    onPersonDirectionsChange={onPersonFileDirectionsChange}
                    onOpenItem={onOpenFileItem}
                    onOpenConversation={onOpenFileConversation}
                  />
                {:else}
                  <RelationshipTimeline
                    rows={controller.timelineRows}
                    loading={controller.timelineLoading}
                    loadingMore={controller.timelineLoadingMore}
                    hasMore={Boolean(controller.timelineCursor)}
                    error={controller.timelineError}
                    restartNotice={controller.timelineRestartNotice}
                    daySelected={controller.timelineDay !== null}
                    selectedKey={selectedRowKey}
                    onRowOpen={openTimelineRow}
                    onLoadMore={() => { void controller.loadMoreTimeline(); }}
                  />
                {/if}
              </div>
            </div>
          {/if}
        </div>
      {/snippet}
      {#snippet secondary()}
        {#if selection !== undefined}
          <div class="pane-reading">
            <ReadingPane
              {client}
              {selection}
              predicate={contextPredicate(predicate)}
              {onOpenMeeting}
              onClose={() => { void closeReadingPane(); }}
              {conversationAnchorId}
              conversationStart={conversationBounds?.start}
              conversationEnd={conversationBounds?.end}
              onConversationAnchorChange={(anchorId) => (conversationAnchorId = anchorId)}
            />
          </div>
        {/if}
      {/snippet}
    </SplitPane>
  </div>
{/snippet}

<!-- svelte-ignore a11y_no_noninteractive_element_interactions -- landmark container scoping the hub's own Esc-layering; not a control itself. -->
<main
  class="relationships-hub"
  aria-label="Relationships"
  class:layout-narrow={layout === 'narrow'}
  bind:this={rootElement}
  onkeydown={handleEscape}
>
  <div class="hub-header">
    <PageHeader title="Relationships" description="People and domains you've exchanged messages with." />
  </div>
  <div class="hub-body">
    {#if layout === 'narrow'}
      <Button
        class="drawer-toggle"
        label="People"
        ariaExpanded={mobileListOpen}
        onclick={() => (mobileListOpen = !mobileListOpen)}
      />
      {@render centerAndReading()}
      {#if mobileListOpen}
        <DetailDrawer
          title="People"
          ariaLabel="Relationship search and results"
          width="min(390px, 100vw)"
          onclose={closeDrawer}
        >
          <div class="pane-list">
            {@render listPane()}
          </div>
        </DetailDrawer>
      {/if}
    {:else}
      <SplitPane
        ariaLabel="Resize relationship list"
        storageKey="msgvault.relationships.list-pane.size"
        initialSize={300}
        minPrimary={240}
        maxPrimary={440}
      >
        {#snippet primary()}
          <div class="pane-list">
            {@render listPane()}
          </div>
        {/snippet}
        {#snippet secondary()}
          {@render centerAndReading()}
        {/snippet}
      </SplitPane>
    {/if}
  </div>
  {#if bulkConflict}
    <PersonBindingConflictModal
      {client}
      conflict={bulkConflict}
      onOpenProfile={(personID) => onOpenDirectoryPerson?.(personID)}
      onSuccess={completeBulkMerge}
      onClose={cancelBulkMerge}
    />
  {/if}
</main>

<style>
  .relationships-hub {
    position: relative;
    display: flex;
    min-height: 0;
    height: 100%;
    flex-direction: column;
    background: var(--bg-canvas);
  }

  .hub-header {
    padding: var(--space-5) var(--page-gutter) var(--space-4);
  }

  .hub-body {
    display: flex;
    min-height: 0;
    flex: 1;
  }

  .pane-list {
    display: flex;
    width: 100%;
    height: 100%;
    flex-direction: column;
    overflow: hidden;
    background: var(--bg-primary);
  }

  .pane-center-and-reading {
    display: flex;
    min-width: 0;
    height: 100%;
    min-height: 0;
    flex: 1;
    overflow: hidden;
  }

  .pane-center {
    display: flex;
    min-width: 0;
    height: 100%;
    flex-direction: column;
    overflow: hidden;
  }

  /* Readable content column: the timeline caps out instead of stretching
   * shapelessly across ultrawide viewports. */
  .pane-center-column {
    display: flex;
    width: 100%;
    max-width: 1080px;
    min-height: 0;
    flex: 1;
    flex-direction: column;
    gap: var(--space-4);
    margin-inline: auto;
    padding: var(--space-6) var(--space-7);
    overflow: auto;
  }

  .hub-empty {
    display: flex;
    flex: 1;
    flex-direction: column;
  }

  /* Keep the activity usable while the calendar and expanded meeting overview
   * scroll independently, including when the reading pane reduces our height. */
  .relationship-overview {
    display: flex;
    flex: 0 1 auto;
    min-height: 80px;
    max-height: 50%;
    flex-direction: column;
    gap: var(--space-4);
    overflow: auto;
  }

  .relationship-overview > :global(*) { flex: none; }

  .relationship-activity {
    display: flex;
    min-width: 0;
    min-height: 240px;
    flex: 1 0 240px;
  }

  .layout-narrow .relationship-activity { min-height: 340px; flex-basis: 340px; }
  .layout-narrow .hub-body { flex-direction: column; }
  .layout-narrow :global(.drawer-toggle) { align-self: flex-start; flex: none; margin-inline: var(--page-gutter); }

  .meeting-overview { flex: none; }
  .meeting-overview summary { cursor: pointer; padding: var(--space-3) 0; color: var(--text-primary); font-size: var(--font-size-sm); font-weight: 600; }
  .meeting-overview[open] summary { margin-bottom: var(--space-2); }

  .pane-reading {
    height: 100%;
    background: var(--bg-surface);
  }
</style>
