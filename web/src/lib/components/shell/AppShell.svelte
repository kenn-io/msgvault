<script lang="ts">
  import { downloadRawMessage } from '../../api/download';
  import { relationshipDestination, tasksSettingsDestination } from './navigation';
  import { preflightExploreSelection as generatedPreflightExploreSelection } from '../../api/generated/exploration/exploration';
  import {
    Button,
    CommandPalette,
    FlashBanner,
    showFlash,
    dismissFlash,
    getThemeMode,
    IconButton,
    PageHeader,
    appShortcuts,
    initShortcuts,
    type PaletteCommand,
  } from '@kenn-io/kit-ui';
  import { onDestroy, onMount, setContext, tick, type Snippet, untrack } from 'svelte';
  import type { APIClient } from '../../api/client';
  import { mediaReadiness } from '../../search/media-ready.svelte';
  import { visualReadiness } from '../../search/visual-ready.svelte';
  import { reportError, reportSearch, reportWhenVisible } from '../../telemetry/actions';
  import { startScreenViewReporting } from '../../telemetry/screen-views';
  import type {
    MeetingRef,
    ExplorePreflightResponse as GeneratedExplorePreflightResponse,
    ExploreSelection as GeneratedExploreSelection,
  } from '../../api/generated/models';
  import type {
    EntryRow,
    ExploreGroupDimension,
    ExploreGroupRow,
    ExploreSearchMode,
    ExploreURLState,
    ExploreWorkspace,
    FileViewerTarget,
    FileSearchSort,
  } from '../../explore/models';
  import { parseAttachmentSelection } from '../../explore/attachment-authority';
  import { filtersForGroup } from '../../explore/group-context';
  import { ExploreLoader, type MessagesTable } from '../../explore/loader.svelte';
  import { GROUPING_CATALOG, groupingByDimension } from '../../grouping/catalog';
  import { canonicalFingerprint, predicateFingerprint } from '../../explore/selection';
  import { ExploreSelectionState, ExploreState, navigationFingerprint } from '../../explore/state.svelte';
  import { contextPredicate, RelationshipsController } from '../../relationships/controller.svelte';
  import { DirectoryController } from '../../directory/controller.svelte';
  import { DirectoryReviewController } from '../../directory/review-controller.svelte';
  import { defaultOperationsState, OperationsController } from '../../operations/controller.svelte';
  import type { OperationSettingsTarget } from '../../operations/labels';
  import type { OperationRunDetail, OperationsURLState } from '../../operations/models';
  import {
    settingsNavigationTarget as targetForSettingsAuthority,
    type CardDAVSettingsRequest,
    type SettingsNavigationTarget
  } from '../../carddav/navigation';
  import { createCommandRegistry, type AppCommand, type CommandHandlers } from '../../commands/registry';
  import {
    createAppearancePreferences,
    type AppearanceDefaults,
    type ThemePreference,
  } from '../../theme/preferences.svelte';
  import ContextBar from '../explore/ContextBar.svelte';
  import GroupTable from '../explore/GroupTable.svelte';
  import SaveViewDialog from '../saved-views/SaveViewDialog.svelte';
  import SourcesWorkspace from '../sources/SourcesWorkspace.svelte';
  import DeletionReview from '../deletions/DeletionReview.svelte';
  import FileFilters from '../files/FileFilters.svelte';
  import { fileFilterChips } from '../files/filter-chips';
  import DeletionsWorkspace from '../deletions/DeletionsWorkspace.svelte';
  import FilesWorkspace from '../files/FilesWorkspace.svelte';
  import FileViewer from '../files/FileViewer.svelte';
  import PeopleWorkspace from '../people/PeopleWorkspace.svelte';
  import KeyboardHelp from './KeyboardHelp.svelte';
  import ArchivedMeetingReader from '../meetings/ArchivedMeetingReader.svelte';
  import { ArchiveMeetingNavigation, archiveMeetingSelection, parseArchiveMeetingSelection } from '../../meetings/archive-navigation.svelte';
  import { ARCHIVE_MEETING_HISTORY_KEY, parseArchiveMeetingHistory } from '../../meetings/archive-selection';
  import { recordingSelection, parseRecordingSelection } from '../../archive/recording-selection';
  import EverythingWorkspace from './EverythingWorkspace.svelte';
  import AppSidebar from './AppSidebar.svelte';
  import DisplayMenu from './DisplayMenu.svelte';
  import NavigationDrawer from './NavigationDrawer.svelte';
  import SearchBar from '../search/SearchBar.svelte';
  import { NAVIGATION_ITEMS, SETTINGS_ITEM, SIDEBAR_COLLAPSED_KEY, workspaceLabel } from './navigation';
  import Menu from '@lucide/svelte/icons/menu';
  import { EverythingSessionState } from './EverythingSessionState.svelte';
  import { bufferedCallback } from '../../util/buffered-callback';

  import { ShellData, viewIncompatibility, viewState } from './shell-data.svelte';
  import { readerPosition } from '../../explore/reader-position.svelte';
  import type { JobTarget } from '../../jobs/jobs';
  import type { SavedView } from '../../api/generated/models';
  import RunHistoryDialog from './RunHistoryDialog.svelte';
  import PersonBindingConflictModal from '../directory/PersonBindingConflictModal.svelte';

  const media = mediaReadiness();
  const visual = visualReadiness();
  interface Props {
    client: APIClient;
    state?: ExploreState;
    enabled?: boolean;
    settings?: Snippet<[
      CardDAVSettingsRequest | undefined,
      (key: number) => void,
      SettingsNavigationTarget | undefined,
      string,
      (categoryID: string) => void,
      () => void,
      (authority: RelatedOperationStatus) => void,
      () => void,
      OperationsController,
      () => Promise<void>
    ]>;
    appearanceDefaults?: AppearanceDefaults;
    searchModeDefault?: ExploreSearchMode;
  }
  let {
    client,
    state: providedState = undefined,
    enabled = true,
    settings = undefined,
    appearanceDefaults = { theme: 'system', density: 'compact' },
    searchModeDefault = undefined,
  }: Props = $props();
  const ownsState = untrack(() => providedState === undefined);
  const exploreState = untrack(() => providedState ?? new ExploreState());
  const reportedWorkspace = $derived(exploreState.current.workspace);
  $effect(() => {
    if (!enabled) return;
    return startScreenViewReporting(client, reportedWorkspace);
  });
  const archivedMeeting = new ArchiveMeetingNavigation(untrack(() => client));
  let archiveReturnFocus: HTMLElement | undefined;
  let archiveReturnSelection = $state<string | null>(null);
  let fileCount = $state<number | null>(null);
  let fileCountLoading = $state(false);
  // Reset before the next render so returning to Files never shows the count it had when it was left.
  $effect.pre(() => {
    void exploreState.current.workspace;
    fileCount = null;
    fileCountLoading = false;
  });
  const FILE_SORTS: Array<FileSearchSort & { label: string }> = [
    { field: 'occurred_at', direction: 'desc', label: 'Newest first' },
    { field: 'occurred_at', direction: 'asc', label: 'Oldest first' },
    { field: 'filename', direction: 'asc', label: 'Filename A–Z' },
    { field: 'filename', direction: 'desc', label: 'Filename Z–A' },
    { field: 'size', direction: 'desc', label: 'Largest first' },
    { field: 'size', direction: 'asc', label: 'Smallest first' },
  ];
  function fileSortValue(sort: FileSearchSort | undefined): string {
    return sort ? `${sort.field}:${sort.direction}` : 'occurred_at:desc';
  }
  const FILE_SORT_OPTIONS = FILE_SORTS.map((sort) => ({ value: fileSortValue(sort), label: sort.label }));
  function commitFileSort(value: string): void {
    const match = FILE_SORTS.find((sort) => fileSortValue(sort) === value);
    if (!match) return;
    commitNavigation({
      fileSort: { field: match.field, direction: match.direction },
      activeRow: null,
      scrollAnchor: null,
    });
  }
  const filesCountLabel = $derived.by(() => {
    if (exploreState.current.groupingChain.length > 0) {
      if (loader.loading) return 'Counting…';
      const groups = loader.error || loader.unavailable ? undefined : loader.result?.totalCount;
      if (groups === undefined) return '';
      return `${groups.toLocaleString()} ${groups === 1 ? 'group' : 'groups'}`;
    }
    if (fileCountLoading) return 'Counting…';
    if (fileCount === null) return '';
    return `${fileCount.toLocaleString()} ${fileCount === 1 ? 'file' : 'files'}`;
  });
  let archiveWasOpen = false;
  const archiveMeetingID = $derived(parseArchiveMeetingSelection(exploreState.current.selectedRow));
  const archiveNavigationFingerprint = $derived(navigationFingerprint(exploreState.current));

  $effect(() => {
    void archiveNavigationFingerprint;
    const id = archiveMeetingID;
    untrack(() => {
      archivedMeeting.cancel();
      if (id !== undefined) {
        const history = parseArchiveMeetingHistory(window.history.state, exploreState.current.selectedRow);
        archiveReturnSelection = history?.returnSelectedRow ?? null;
        if (archivedMeeting.detail?.id !== id) void archivedMeeting.load(id);
      } else {
        archivedMeeting.error = '';
        if (archiveWasOpen) void restoreArchiveFocus();
      }
      archiveWasOpen = id !== undefined;
    });
  });
  const DEFAULT_SORT_NOTICE = 'Newest first is the canonical Messages order.';
  let fileFiltersReset = $state(0);
  const SEARCH_TYPING_DEBOUNCE_MS = 250;
  const debouncedSearchPatch = bufferedCallback((patch: Partial<ExploreURLState>) => {
    exploreState.replaceCommittedDraft(patch);
  }, SEARCH_TYPING_DEBOUNCE_MS);
  function beforeCommit(): void {
    // Commit pending search text before navigation captures the current state.
    debouncedSearchPatch.flush();
  }
  function replaceTransient(patch: Partial<ExploreURLState>): void {
    // Apply pending search text before focus or scroll updates replace this entry.
    debouncedSearchPatch.flush();
    exploreState.replaceTransient(patch);
  }
  function commitNavigation(patch: Partial<ExploreURLState>): void {
    beforeCommit();
    exploreState.commitNavigation(patch);
  }
  function replaceCommittedNavigation(patch: Partial<ExploreURLState>): void {
    beforeCommit();
    exploreState.replaceCommittedNavigation(patch);
  }
  // Directory text filters are committed drafts like the Everything typed
  // search: they rewrite the current history entry instead of pushing one.
  function replaceCommittedDraft(patch: Partial<ExploreURLState>): void {
    beforeCommit();
    exploreState.replaceCommittedDraft(patch);
  }
  function commitRestorableNavigation(patch: Partial<ExploreURLState>): void {
    beforeCommit();
    exploreState.commitRestorableNavigation(patch);
  }
  function replaceCommittedRestorableNavigation(patch: Partial<ExploreURLState>): void {
    beforeCommit();
    exploreState.replaceCommittedRestorableNavigation(patch);
  }
  function commitWorkspace(workspace: ExploreWorkspace): void {
    beforeCommit();
    exploreState.commitWorkspace(workspace);
  }
  function openDirectoryPerson(personID: number): void {
    beforeCommit();
    exploreState.commitNavigation({ workspace: 'relationships', directoryPersonID: personID, peopleDuplicates: false, relationshipTarget: null, relationshipFiles: false });
  }
  function announceOperation(message: string): void {
    operationAnnouncement = { key: ++operationAnnouncementKey, message };
  }
  function openCardDAVConflict(conflictID: number): void {
    if (!Number.isSafeInteger(conflictID) || conflictID <= 0) return;
    cardDAVSettingsRequest = { conflictID, key: ++cardDAVSettingsRequestKey };
    announceOperation(`Opening CardDAV conflict ${conflictID} in Settings.`);
    commitWorkspace('settings');
    replaceCommittedNavigation({ settingsCategory: 'carddav' });
  }
  function selectSettingsCategory(categoryID: string): void {
    commitNavigation({ settingsCategory: categoryID, settingsAuthority: '' });
  }
  function openCardDAVSettings(): void {
    cardDAVSettingsRequest = { key: ++cardDAVSettingsRequestKey };
    announceOperation('Opening CardDAV settings.');
    commitWorkspace('settings');
    replaceCommittedNavigation({ settingsCategory: 'carddav' });
  }
  let historyDialog = $state<{ restoreFocus: () => Promise<void> }>();
  let sourcesPage = $state<{ refresh: () => void }>();
  const runHistory = $derived(exploreState.current.operationHistory
    ? { operationLane: exploreState.current.operationLane, operationKind: exploreState.current.operationKind, operationState: exploreState.current.operationState, operationStartedFrom: exploreState.current.operationStartedFrom, operationStartedBefore: exploreState.current.operationStartedBefore, operationRunID: exploreState.current.operationRunID, operationStatus: exploreState.current.operationStatus }
    : undefined);
  function setRunHistory(state: OperationsURLState): void {
    beforeCommit();
    exploreState.openOperationHistory(state);
  }
  function closeRunHistory(): void {
    beforeCommit();
    exploreState.closeOperationHistory();
  }
  let displayOpen = $state(false);
  let personMessages = $state<MessagesTable>();
  let resultsWidth = $state(0);
  const operationsController = new OperationsController(untrack(() => client));
  const shellData = new ShellData(untrack(() => client), operationsController);

  function openOperations(kind: OperationsURLState['operationKind'] = ''): void {
    setRunHistory(defaultOperationsState(kind));
  }
  function openJob(target: JobTarget): void {
    if (target.kind === 'source' || target.kind === 'status') openOperationAuthority(target.kind === 'source' ? 'listSourceStatus' : target.authority);
    else if (target.kind === 'setting') setUpOperation(target.target);
    else setRunHistory({ ...defaultOperationsState(target.operationKind), operationRunID: target.operationRunID });
  }
  async function flashViewError(result: Promise<string | undefined>): Promise<void> {
    const error = await result;
    if (error) showFlash(error, { tone: 'danger', durationMs: 8000 });
  }
  function flashDecisionError(message: string): void {
    if (exploreState.current.workspace === 'relationships' && exploreState.current.peopleDuplicates) announceOperation(message);
    else showFlash(message, { tone: 'danger', durationMs: 8000, action: { label: 'Review duplicates', onclick: () => commitNavigation({ workspace: 'relationships', peopleDuplicates: true }) } });
  }
  type RelatedOperationStatus = NonNullable<OperationRunDetail['related_status']>;

  // Past runs links to another workspace drop the reader selection, as a workspace
  // switch does, so a meeting under the dialog does not reappear over the destination.
  function leaveRunHistory(patch: Partial<ExploreURLState>): void {
    commitNavigation({ ...defaultOperationsState(), operationHistory: false, selectedRow: null, conversationAnchor: null, ...patch });
  }

  function openOperationAuthority(target: RelatedOperationStatus): void {
    if (target === 'listSourceStatus' || target === 'getCardDAVStatus') {
      announceOperation(target === 'listSourceStatus' ? 'Opening source status.' : 'Opening CardDAV settings.');
      leaveRunHistory({ workspace: target === 'listSourceStatus' ? 'sources' : 'settings',
        ...(target === 'getCardDAVStatus' ? { settingsCategory: 'carddav', settingsAuthority: '' } : {}) });
      if (target === 'listSourceStatus') sourcesPage?.refresh();
      return;
    }
    const statusTargets: Record<
      Exclude<RelatedOperationStatus, 'listSourceStatus' | 'getCardDAVStatus'>,
      string
    > = {
      getDocumentIndexStatus: 'Opening live document index status.',
      getDocumentVectorStatus: 'Opening live document vector status.',
      getVisualAttachmentStatus: 'Opening live visual attachment status.'
    };
    announceOperation(statusTargets[target]);
    setRunHistory({ ...(runHistory ?? defaultOperationsState()), operationStatus: target });
  }

  function openVisualAttachmentSettings(): void {
    leaveRunHistory({ workspace: 'settings', settingsCategory: 'search', settingsAuthority: 'visual_attachments' });
  }

  function setUpOperation(target: OperationSettingsTarget): void {
    leaveRunHistory({ workspace: 'settings', ...target });
  }

  setContext('msgvault:open-carddav-operations', () => openOperations('carddav_sync'));
  setContext('msgvault:hold-settings-leave', (hold: ExploreState['holdLeave'], cancel: ExploreState['cancelHeldLeave']) => {
    exploreState.holdLeave = hold;
    exploreState.cancelHeldLeave = cancel;
  });
  function consumeCardDAVSettingsRequest(key: number): void {
    if (cardDAVSettingsRequest?.key === key) cardDAVSettingsRequest = undefined;
  }
  function openWorkspaceTab(workspace: ExploreWorkspace): void {
    if (workspace === 'settings') cardDAVSettingsRequest = undefined;
    commitWorkspace(workspace);
  }
  function commitGrouping(dimension: ExploreGroupDimension): void {
    beforeCommit();
    exploreState.commitGrouping(dimension);
  }
  function commitUngroup(): void {
    beforeCommit();
    exploreState.commitUngroup();
  }
  function commitSearch(query: string, mode: ExploreSearchMode): void {
    beforeCommit();
    exploreState.commitSearch(query, mode);
  }
  const selection = new ExploreSelectionState();
  const appearance = createAppearancePreferences(untrack(() => appearanceDefaults));
  const relationshipsController = new RelationshipsController(
    untrack(() => client),
    () => Intl.DateTimeFormat().resolvedOptions().timeZone,
  );
  // Directory owns ephemeral request/page state while ExploreState remains
  // the browser-restorable source for its query, filters, and selection.
  // Keeping the commit callback at the shell boundary gives it the same
  // history treatment as every other workspace navigation.
  const directoryController = new DirectoryController(
    untrack(() => client),
    (patch, history) => (history === 'replace' ? replaceCommittedDraft(patch) : commitNavigation(patch)),
    () => { void relationshipsController.loadList(exploreState.predicate()); void identityChanged(); },
  );
  const directoryReviewController = new DirectoryReviewController(
    untrack(() => client),
    (patch) => commitNavigation(patch),
    async result => {
      if (!result.ok && result.kind !== 'merge_required') flashDecisionError(result.message);
      if (result.ok) await reconcileIdentity();
    },
  );
  async function reconcileIdentity(): Promise<void> {
    await Promise.all([shellData.refreshDuplicates(), directoryController.loadFirstPage(), directoryController.refreshSelectedPerson(), relationshipsController.reconcilePersonMerge(relationshipsController.personMergeContextSnapshot())]);
  }
  async function identityChanged(): Promise<void> {
    await Promise.all([
      shellData.refreshDuplicates(),
      directoryReviewController.invalidate(exploreState.current.workspace === 'relationships' && exploreState.current.peopleDuplicates),
    ]);
  }
  let cardDAVSettingsRequest = $state<CardDAVSettingsRequest>();
  let cardDAVSettingsRequestKey = 0;
  const settingsNavigationTarget = $derived(exploreState.current.workspace === 'settings'
    ? targetForSettingsAuthority(exploreState.current.settingsAuthority)
    : undefined);
  let operationAnnouncementKey = 0;
  let operationAnnouncement = $state({ key: 0, message: '' });
  let saveViewOpen = $state(false);
  type APIExploreSelection = GeneratedExploreSelection;
  type ExplorePreflight = GeneratedExplorePreflightResponse;
  const NARROW_WIDTH = 900;
  let viewportWidth = $state(typeof window === 'undefined' ? 1280 : window.innerWidth);
  const narrow = $derived(viewportWidth < NARROW_WIDTH);
  let drawerOpen = $state(false);
  let drawerOpenerHost = $state<HTMLElement>();
  let shellRoot = $state<HTMLElement>();
  let flashRoot = $state<HTMLElement>();
  $effect(() => {
    const view = shellData.pending;
    if (view) return untrack(() => stageUndo(`Deleted ${view.name}`, () => undoViewDeletion(view), () => void flashViewError(shellData.expireDelete())));
  });
  $effect(() => {
    const queued = directoryReviewController.pending;
    if (queued) return untrack(() => stageUndo(queued.decision === 'accept' ? 'Merge queued' : 'Keep separate queued', () => undoDuplicateDecision(queued.candidate.id), () => void directoryReviewController.settleDecision()));
  });

  /** The flash only offers Undo; the timer and pagehide make the staged change final. */
  function stageUndo(message: string, undo: () => Promise<void>, expire: () => void): () => void {
    const flashID = showFlash(message, { durationMs: 8000, action: { label: 'Undo', onclick: () => void undo() } });
    const timer = setTimeout(expire, 8000);
    const refocus = (target: () => HTMLElement | null | undefined) => void tick().then(() => document.activeElement === document.body && target()?.focus());
    window.addEventListener('pagehide', expire);
    refocus(() => [...flashRoot?.querySelectorAll<HTMLElement>('.kit-flash-banner__action') ?? []].at(-1));
    return () => {
      clearTimeout(timer);
      window.removeEventListener('pagehide', expire);
      dismissFlash(flashID);
      refocus(() => shellRoot?.querySelector<HTMLElement>('[role="radio"][aria-checked="true"]') ?? navigationFocusTarget());
    };
  }

  async function undoViewDeletion(view: SavedView): Promise<void> {
    await shellData.undoDelete();
    await tick();
    shellRoot?.querySelector<HTMLElement>(`[data-saved-view-id="${view.id}"] .saved-view__open`)?.focus();
  }

  async function undoDuplicateDecision(candidateID: number): Promise<void> {
    directoryReviewController.undoDecision();
    await tick();
    document.getElementById(`duplicate-${candidateID}`)?.focus();
  }

  let sidebarCollapsed = $state(readSidebarCollapsed());

  function readSidebarCollapsed(): boolean {
    try {
      return localStorage.getItem(SIDEBAR_COLLAPSED_KEY) === 'true';
    } catch {
      return false;
    }
  }

  function toggleSidebar(): void {
    sidebarCollapsed = !sidebarCollapsed;
    try {
      localStorage.setItem(SIDEBAR_COLLAPSED_KEY, String(sidebarCollapsed));
    } catch {
      // Storage may be disabled; the rail still toggles for this page view.
    }
  }

  function drawerOpener(): HTMLElement | null {
    return drawerOpenerHost?.querySelector('button') ?? null;
  }

  // Below NARROW_WIDTH the sidebar is not rendered, so the drawer opener
  // stands in for the current navigation item.
  function navigationFocusTarget(): HTMLElement | null {
    const navigation = document.querySelector<HTMLElement>('button[aria-current="page"]') ?? drawerOpener();
    if (navigation) return navigation;
    const heading = shellRoot?.querySelector<HTMLElement>('.app-main h1') ?? null;
    heading?.setAttribute('tabindex', '-1');
    return heading;
  }

  // trapFocus restores focus while the page column may still be inert, so
  // focus the opener again once the drawer has unmounted.
  async function closeDrawer(): Promise<void> {
    drawerOpen = false;
    await tick();
    drawerOpener()?.focus();
  }

  $effect(() => {
    if (!narrow) drawerOpen = false;
  });

  $effect(() => {
    document.title = `${workspaceLabel(exploreState.current.workspace)} · msgvault`;
  });

  $effect(() => {
    if (exploreState.current.workspace !== 'settings') cardDAVSettingsRequest = undefined;
  });

  $effect(() => {
    const active = enabled;
    untrack(() => { if (active) void shellData.refresh(); });
  });
  onMount(() => {
    shellData.start();
    if (enabled) void shellData.jobs.refresh();
    const refresh = () => { if (enabled) { void shellData.refresh(); void shellData.jobs.refresh(); void visual?.ready; } };
    window.addEventListener('focus', refresh);
    return () => { window.removeEventListener('focus', refresh); shellData.stop(); };
  });
  let contextualViewerFile = $state<FileViewerTarget>();
  let contextualViewerReturnFocus = $state<HTMLElement>();
  let previousAttachmentID: number | undefined;
  let selectionPreflight = $state<ExplorePreflight>();
  let selectionPreflightController: AbortController | undefined;
  let pendingDeletionReview = $state<GeneratedExploreSelection>();
  let searchInput = $state<HTMLInputElement>();
  // The loader also drives the Files-shell grouped view (AppShell gates its
  // internal load effect on `workspace === 'files' && groupingChain.length >
  // 0` in addition to `workspace === 'everything'`), so it is owned here
  // rather than by EverythingWorkspace alone. `sortNotice`, `selection`, and
  // grid focus restoration are shared shell-wide state the loader cannot own
  // itself; it reaches them only through these callbacks, called at the
  // exact points the original inline effect wrote to that shared state.
  const loader = new ExploreLoader(
    untrack(() => client),
    exploreState,
    {
      isEnabled: () => enabled,
      onPredicateChange: () => selection.clear(),
      onPagingNotice: (message) => {
        sortNotice = message ?? DEFAULT_SORT_NOTICE;
      },
      onRestorationFocus: () => focusGrid(),
      onSearchAccepted: (query, mode, signal, count) => { reportWhenVisible(() => reportSearch(client, query, mode, exploreState.current.workspace === 'everything' && !media?.absent ? undefined : count), signal); },
    },
  );
  // Owned here (not by EverythingWorkspace) so coverage-poll backoff, the
  // exact lexical match-count cache, and loaded reading-pane group detail
  // survive a workspace round-trip: AppShell renders EverythingWorkspace
  // behind an {#if}, so it is destroyed and recreated on every switch away
  // from and back to 'everything'.
  const everythingSession = new EverythingSessionState();
  $effect(() => { if (archivedMeeting.error) return reportError(() => client, () => 'message'); });
  $effect(() => { if ((exploreState.current.workspace === 'everything' || exploreState.current.workspace === 'files' && exploreState.current.groupingChain.length > 0) && (loader.error || loader.pageError || loader.unavailable && loader.unavailable.readiness !== 'building')) return reportError(() => client, () => loader.errorKind ?? (exploreState.current.query.trim() ? 'search' : 'other')); });
  function submitGlobalSearch(query: string, mode: ExploreSearchMode): void {
    loader.submitted = true;
    loader.retry();
    const workspace = exploreState.current.workspace;
    if (workspace === 'relationships') { commitNavigation({ identityQuery: query }); return; }
    if (workspace === 'everything' || workspace === 'files') {
      commitSearch(query, mode);
      focusGrid();
      return;
    }
    beforeCommit();
    exploreState.commitSearchIn('everything', query, mode);
  }
  let paletteOpen = $state(false);
  let keyboardHelpOpen = $state(false);
  let keyboardHelpReturnFocus: HTMLElement | undefined;
  let keyboardHelpScopeCleanup: (() => void) | undefined;
  let sortNotice = $state(DEFAULT_SORT_NOTICE);
  let editableScopeCleanup: (() => void) | undefined;
  let previousSortNoticeWorkspace = untrack(() => exploreState.current.workspace);
  // An epoch is a monotonically increasing restoration event, not a boolean
  // mode. Capture the initial value so ordinary URL commits do not turn into
  // repeated page-one reloads merely because the initial epoch is positive.
  let appliedDirectoryReviewContext = untrack(() => ({ epoch: exploreState.restorationEpoch, active: false }));
  $effect(() => {
    const defaults = appearanceDefaults;
    untrack(() => appearance.setDefaults(defaults));
  });
  // URL restoration is shell-owned, just like Relationships' target
  // hydration below. DirectoryWorkspace also supports isolated mounting for
  // component tests, but normal app navigation reaches the controller here.
  $effect(() => {
    const restorationEpoch = exploreState.restorationEpoch;
    const active = exploreState.current.workspace === 'relationships' && exploreState.current.peopleDuplicates;
    const historyRestoration = !appliedDirectoryReviewContext.active || restorationEpoch !== appliedDirectoryReviewContext.epoch;
    appliedDirectoryReviewContext = { epoch: restorationEpoch, active };
    const identityState = exploreState.current.identityState;
    untrack(() => {
      void directoryReviewController.settleDecision();
      if (active) directoryReviewController.applyURLState({ identityState }, historyRestoration);
    });
  });
  $effect(() => {
    const mode = getThemeMode();
    if (mode === appearance.current.theme) return;
    untrack(() => appearance.setTemporary({ theme: mode as ThemePreference }));
  });
  $effect(() => {
    const mode = searchModeDefault;
    untrack(() => exploreState.setConfiguredDefaultSearchMode(mode));
  });
  // sortNotice is shared across workspaces (Everything, Files, etc.). A
  // workspace-specific notice (e.g. the Files grouped End-cap pause message)
  // must not leak into another workspace after switching — for example via
  // the ContextBar "Show as" presentation control, which changes the
  // workspace through commitNavigation rather than commitWorkspace. Resetting
  // here, keyed off the actual workspace value rather than the commit path,
  // covers every route that changes it. Ordinary paging/sorting within the
  // same workspace must not clear the notice, so this only fires on a change.
  $effect(() => {
    const workspace = exploreState.current.workspace;
    untrack(() => {
      if (workspace !== previousSortNoticeWorkspace) {
        previousSortNoticeWorkspace = workspace;
        sortNotice = DEFAULT_SORT_NOTICE;
      }
    });
  });
  const selectedAttachmentID = $derived(parseAttachmentSelection(exploreState.current.selectedRow));
  const readingTargetKey = $derived(archiveMeetingID !== undefined ? archiveReturnSelection : selectedAttachmentID === undefined ? exploreState.current.selectedRow : null);
  const apiSelection = $derived.by((): APIExploreSelection | undefined => {
    const snapshot = selection.snapshot();
    const authority = loader.result;
    if (snapshot.mode === 'explicit') {
      if (snapshot.rowKeys.length === 0 || !authority) return undefined;
      return {
        mode: 'explicit',
        predicate: exploreState.predicate(),
        row_keys: snapshot.rowKeys,
        cache_revision: authority.cacheRevision,
        search_provenance: authority.searchProvenance,
        ...(authority.candidateSnapshotId ? { candidate_snapshot_id: authority.candidateSnapshotId } : {}),
      };
    }
    return {
      mode: 'all_matching',
      predicate: snapshot.predicate,
      exclusions: snapshot.exclusions,
      cache_revision: snapshot.cacheRevision,
      search_provenance: snapshot.searchProvenance,
      ...(snapshot.candidateSnapshotId ? { candidate_snapshot_id: snapshot.candidateSnapshotId } : {}),
    };
  });
  const apiSelectionFingerprint = $derived(canonicalFingerprint(apiSelection));
  $effect(() => {
    const workspace = exploreState.current.workspace;
    void apiSelectionFingerprint;
    const candidate = untrack(() => apiSelection);
    selectionPreflightController?.abort();
    selectionPreflightController = undefined;
    selectionPreflight = undefined;
    if (!candidate || workspace !== 'everything') return;
    const controller = new AbortController();
    selectionPreflightController = controller;
    void generatedPreflightExploreSelection(
      { selection: candidate },
      {
        ...client,
        signal: controller.signal,
      },
    )
      .then(({ data }) => {
        if (!controller.signal.aborted) selectionPreflight = data;
      })
      .catch(() => undefined);
  });
  async function exportSelection(): Promise<void> {
    const target = selectionPreflight?.action_targets.find((item) => item.action === 'export');
    if (!target || !apiSelection || selectionPreflight?.count !== 1) return;
    try {
      await downloadRawMessage(client, target.message_id, target.filename);
    } catch (cause) {
      loader.errorKind = 'other';
      loader.error = cause instanceof Error ? cause.message : 'Unable to export the selected message.';
    }
  }
  function openDeletionReview(mode: 'explicit' | 'all_matching'): void {
    const candidate = apiSelection;
    if (exploreState.current.workspace !== 'everything' || !candidate || candidate.mode !== mode) return;
    pendingDeletionReview = candidate;
  }
  async function openSavedView(state: Partial<ExploreURLState>): Promise<void> {
    if (exploreState.holdNavigation(state, () => void openSavedView(state))) return;
    selection.clear();
    commitNavigation(state);
    if (exploreState.current.workspace === 'settings') return;
    await tick();
    (focusableResultsGrid() ?? searchInput)?.focus();
  }
  $effect(() => {
    const attachmentID = selectedAttachmentID;
    if (attachmentID === undefined) {
      contextualViewerFile = undefined;
      if (previousAttachmentID !== undefined) {
        void tick().then(() => (contextualViewerReturnFocus ?? focusableResultsGrid())?.focus());
      }
      previousAttachmentID = undefined;
      return;
    }
    previousAttachmentID = attachmentID;
    if (!untrack(() => contextualViewerReturnFocus)) {
      contextualViewerReturnFocus = focusableResultsGrid() ?? undefined;
    }
    if (untrack(() => contextualViewerFile)?.id !== attachmentID) {
      contextualViewerFile = { id: attachmentID };
    }
  });
  const conversationAnchorId = $derived.by(() => {
    const anchor = exploreState.current.conversationAnchor;
    if (anchor === null) return undefined;
    const parsed = Number(anchor);
    return Number.isSafeInteger(parsed) && parsed > 0 ? parsed : undefined;
  });
  function editableTarget(target: EventTarget | null): boolean {
    if (!(target instanceof Element)) return false;
    const element = target as HTMLElement;
    return Boolean(
      element?.closest('input, textarea, select, [contenteditable]:not([contenteditable="false"]), iframe'),
    );
  }
  function preserveNativeControlKey(event: KeyboardEvent): void {
    if (!(event.target instanceof Element)) return;
    const target = event.target;
    const activationControl = target.closest('button, a[href], summary, [role="button"], [role="option"]');
    const directionalControl = target.closest('[role="radio"], [role="option"], select');
    if (
      (activationControl && (event.key === 'Enter' || event.key === ' ')) ||
      (directionalControl && event.key.startsWith('Arrow'))
    ) {
      // Local control handlers and browser defaults run before this document
      // listener; stop only the app-wide shortcut listener on window.
      event.stopPropagation();
    }
  }
  function syncEditableShortcutScope(target: EventTarget | null): void {
    const focused = editableTarget(target);
    if (focused && appShortcuts.activeScope() === 'everything-editable') return;
    if (!focused && !editableScopeCleanup) return;
    editableScopeCleanup?.();
    editableScopeCleanup = focused ? appShortcuts.pushScope('everything-editable') : undefined;
  }
  function focusGrid(): void {
    if (document.querySelector('[aria-modal="true"]')) return;
    focusableResultsGrid()?.focus();
  }
  async function restoreHistoryFocus(): Promise<void> {
    await tick();
    if (runHistory) {
      await historyDialog?.restoreFocus();
      return;
    }
    if (exploreState.current.workspace === 'everything') {
      if (exploreState.current.selectedRow === null) focusGrid();
      return;
    }
    if (exploreState.current.workspace === 'settings' && document.activeElement?.closest('.app-main')) return;
    const grid = exploreState.current.workspace === 'files' && exploreState.current.selectedRow === null
      ? focusableResultsGrid() : null;
    (grid ?? navigationFocusTarget())?.focus();
  }
  async function focusGridAfterUpdate(): Promise<void> {
    await tick();
    focusGrid();
  }
  function filesGrid(): HTMLElement | null {
    return document.querySelector<HTMLElement>('[role="grid"][aria-label="Files results"]');
  }
  // Shortcut relays target this grid. The ungrouped Files grid handles its own keys, so it is
  // left out; grouped Files uses the same GroupTable as grouped Everything.
  function currentGrid(): HTMLElement | null {
    return document.querySelector<HTMLElement>([
      '[role="grid"][aria-label="Messages results"]',
      '[role="grid"][aria-label^="Messages grouped by"]',
      '[role="grid"][aria-label^="Files grouped by"]',
    ].join(', '));
  }
  function focusableResultsGrid(): HTMLElement | null {
    return currentGrid() ?? filesGrid();
  }
  function relayGridKey(event: KeyboardEvent, key: string, init: KeyboardEventInit = {}): void {
    if (event.target instanceof Element && event.target.closest('button, a, summary, [role="button"]')) return;
    const grid = currentGrid();
    if (!grid || event.target === grid || (event.target instanceof Element && event.target.closest('[role="grid"]') && event.target.closest('[role="grid"]') !== grid)) return;
    grid.focus();
    grid.dispatchEvent(new KeyboardEvent('keydown', { key, bubbles: true, cancelable: true, ...init }));
  }
  async function closeReadingPane(): Promise<void> {
    const recordingOpen = parseRecordingSelection(exploreState.current.selectedRow) !== undefined;
    commitNavigation({ selectedRow: null });
    if (recordingOpen) {
      await restoreArchiveFocus(document.querySelector<HTMLElement>('[data-recording-hits]') ?? undefined);
      return;
    }
    await tick();
    focusGrid();
  }
  function openRecording(messageID: number): void {
    archiveReturnFocus = document.activeElement instanceof HTMLElement ? document.activeElement : undefined;
    commitNavigation({ selectedRow: recordingSelection(messageID), conversationAnchor: null });
  }
  async function openArchivedMeeting(meeting: MeetingRef): Promise<void> {
    const origin = navigationFingerprint(exploreState.current);
    const returnSelectedRow = archiveMeetingID !== undefined ? archiveReturnSelection : exploreState.current.selectedRow;
    if (archiveMeetingID === undefined) archiveReturnFocus = document.activeElement instanceof HTMLElement ? document.activeElement : undefined;
    const message = await archivedMeeting.load(meeting.message_id, meeting.conversation_id);
    if (!message || origin !== navigationFingerprint(exploreState.current)) return;
    const open = () => {
      archiveReturnSelection = returnSelectedRow;
      commitRestorableNavigation({ selectedRow: archiveMeetingSelection(message.id), conversationAnchor: null });
      window.history.replaceState({
        ...window.history.state,
        [ARCHIVE_MEETING_HISTORY_KEY]: { id: message.id, returnSelectedRow }
      }, '', window.location.href);
    };
    // Past runs owns the history entries above where it opened, and closing it
    // rewinds past them, so a meeting that loads behind it opens after it closes.
    if (exploreState.current.operationHistory) meetingBehindRunHistory = { origin, open };
    else open();
  }
  let meetingBehindRunHistory: { origin: string; open: () => void } | undefined;
  $effect(() => {
    if (exploreState.current.operationHistory) return;
    untrack(() => {
      const pending = meetingBehindRunHistory;
      meetingBehindRunHistory = undefined;
      if (pending?.origin === navigationFingerprint(exploreState.current)) pending.open();
    });
  });

  async function restoreArchiveFocus(fallback: HTMLElement | undefined = undefined): Promise<void> {
    await tick();
    // Kit releases its focus trap during teardown; focus the surviving source
    // link after that cleanup (or the current workspace's own control).
    await new Promise<void>((resolve) => setTimeout(resolve, 0));
    const modal = Array.from(document.querySelectorAll<HTMLElement>('[aria-modal="true"]')).at(-1);
    const sourceHref = archiveReturnFocus?.getAttribute('href');
    const sourceLink = sourceHref && modal
      ? Array.from(modal.querySelectorAll<HTMLAnchorElement>('a[href]')).find((link) => link.getAttribute('href') === sourceHref)
      : undefined;
    const target = archiveReturnFocus?.isConnected
      ? archiveReturnFocus
      : sourceLink ?? modal ?? fallback ?? focusableResultsGrid() ?? navigationFocusTarget();
    target?.focus();
  }

  function closeArchivedMeeting(): void {
    if (parseArchiveMeetingHistory(window.history.state, exploreState.current.selectedRow)) {
      window.history.back();
    } else {
      replaceCommittedNavigation({ selectedRow: null, conversationAnchor: null });
    }
  }

  function openFileItem(entryKey: string): void {
    if (selectedAttachmentID !== undefined) {
      replaceCommittedRestorableNavigation({
        workspace: 'everything',
        presentation: 'table',
        selectedRow: entryKey,
        conversationAnchor: null,
        activeRow: null,
        scrollAnchor: null,
      });
      return;
    }
    commitRestorableNavigation({
      workspace: 'everything',
      presentation: 'table',
      selectedRow: entryKey,
      conversationAnchor: null,
      activeRow: null,
      scrollAnchor: null,
    });
  }
  function openFileConversation(entryKey: string, messageID: number, _conversationID: number): void {
    if (selectedAttachmentID !== undefined) {
      replaceCommittedRestorableNavigation({
        workspace: 'everything',
        presentation: 'table',
        selectedRow: entryKey,
        conversationAnchor: String(messageID),
        activeRow: null,
        scrollAnchor: null,
      });
      return;
    }
    commitRestorableNavigation({
      workspace: 'everything',
      presentation: 'table',
      selectedRow: entryKey,
      conversationAnchor: String(messageID),
      activeRow: null,
      scrollAnchor: null,
    });
  }
  async function closeContextualViewer(): Promise<void> {
    replaceCommittedNavigation({ selectedRow: null });
    contextualViewerFile = undefined;
    await tick();
    (contextualViewerReturnFocus ?? focusableResultsGrid())?.focus();
  }
  function changeConversationAnchor(anchorId: number): void {
    replaceCommittedNavigation({ conversationAnchor: String(anchorId) });
  }
  let claimedEscape: KeyboardEvent | undefined;
  // The Relationships hub owns its own Esc layering (reading pane → timeline
  // → list) and only lets an Esc it didn't consume bubble here once it has
  // nothing left to close. This function's branches read/write state that
  // belongs to Everything/Files (selectedRow, groupingChain, the conversation
  // and attachment viewers) — none of it relevant to the hub — so a bubbled
  // Esc must not fall through into clearing leftover state from a workspace
  // the user isn't even in (e.g. a groupingChain left behind by
  // commitWorkspace, which does not reset it).
  function handleEscape(event: KeyboardEvent): void {
    if (event === claimedEscape) return;
    if (editableTarget(event.target)) return;
    if (exploreState.current.workspace === 'relationships') return;
    if (selectedAttachmentID !== undefined) {
      void closeContextualViewer();
    } else if (exploreState.current.selectedRow !== null) {
      // Closing the reading pane clears any in-thread anchor with it —
      // the thread is the pane's default content, not a separate layer.
      void closeReadingPane();
    } else if (exploreState.current.groupingChain.length > 0) {
      commitUngroup();
    }
    focusGrid();
  }
  function openContextControl(kind: 'filters' | 'grouping' | 'sort'): void {
    const selector = { filters: 'button[aria-label="Filter"], button[aria-label="Filters"]', grouping: '[data-group-picker] button', sort: '[data-sort-menu] button' }[kind];
    const control = (document.querySelector('.everything-workspace') ?? document).querySelector<HTMLButtonElement>(selector);
    control?.focus();
    control?.click();
  }
  function fixedSortNotice(): void {
    sortNotice = exploreState.current.groupingChain.length > 0
      ? 'Sorting isn’t available while grouped.'
      : exploreState.current.workspace === 'files'
        ? 'Use the Sort menu to change the order.'
        : 'Messages is always shown newest first.';
    document.querySelector<HTMLButtonElement>('[data-sort-menu] button')?.focus();
  }
  function navigateReader(delta: number, { state, loader: { rows }, openRow: open }: MessagesTable = personMessages ?? { state: exploreState, loader, openRow }): void {
    if (!state.current.selectedRow || rows.length === 0) return;
    const index = rows.findIndex((row) => row.key === state.current.selectedRow);
    if (index < 0) return;
    const next = rows[Math.max(0, Math.min(rows.length - 1, index + delta))];
    if (next && next.key !== state.current.selectedRow) open(next);
  }
  function relay(event: KeyboardEvent | undefined, key: string | undefined = undefined, init: KeyboardEventInit = {}): void {
    const resolvedKey = key ?? event?.key;
    if (!resolvedKey) return;
    if (event) {
      relayGridKey(event, resolvedKey, init);
      return;
    }
    queueMicrotask(() => {
      const grid = currentGrid();
      if (!grid) return;
      grid.focus();
      grid.dispatchEvent(new KeyboardEvent('keydown', { key: resolvedKey, bubbles: true, cancelable: true, ...init }));
    });
  }
  const commandHandlers: CommandHandlers = {
    'move-next': (event) => relay(event, 'j'),
    'move-previous': (event) => relay(event, 'k'),
    'reader-previous': () => navigateReader(-1),
    'reader-next': () => navigateReader(1),
    'page-up': (event) => relay(event, 'PageUp'),
    'page-down': (event) => relay(event, 'PageDown'),
    'first-row': (event) => relay(event, 'Home'),
    'last-row': (event) => relay(event, 'End'),
    'open-row': (event) => relay(event, 'Enter'),
    'close-layer': (event) => handleEscape(event ?? new KeyboardEvent('keydown', { key: 'Escape' })),
    'focus-search': (event) => {
      if (!editableTarget(event?.target ?? null)) searchInput?.focus();
    },
    'toggle-selection': (event) => relay(event, ' '),
    'extend-selection': (event) => relay(event, ' ', { shiftKey: true }),
    'select-visible': (event) => relay(event, 'A'),
    'clear-selection': (event) => {
      if (event) relay(event, 'x');
      else {
        selection.clear();
        queueMicrotask(focusGrid);
      }
    },
    'review-delete-selected': () => openDeletionReview('explicit'),
    'review-delete-matching': () => openDeletionReview('all_matching'),
    'open-filters': () => openContextControl('filters'),
    'open-grouping': () => openContextControl('grouping'),
    'change-sort': () => openContextControl('sort'),
    'reverse-sort': fixedSortNotice,
    'open-keyboard-help': openKeyboardHelp,
    'open-command-palette': (event) => {
      if (!editableTarget(event?.target ?? null)) paletteOpen = true;
    },
  };
  const groupingCommands = $derived(
    GROUPING_CATALOG.flatMap((entry) => {
      if (!entry.requestable)
        return [
          {
            id: `unavailable:${entry.concept}`,
            label: `Group by ${entry.label} (not available yet)`,
            section: 'Group by',
            keywords: `${entry.keywords} ${entry.unavailableReason ?? ''}`,
            keys: [],
            combos: [],
            scopes: [],
            destructive: false,
            review: false,
            disabled: true,
            run: () => undefined,
          },
        ];
      return entry.requestDimensions.map((dimension) => ({
        id: `group:${dimension}`,
        label: `Group by ${dimension === 'year' ? 'Year' : dimension === 'month' ? 'Month' : entry.label}`,
        section: 'Group by',
        keywords: entry.keywords,
        keys: [],
        combos: [],
        scopes: [],
        destructive: false,
        review: false,
        disabled: exploreState.current.groupingChain.includes(dimension),
        run: () => {
          commitGrouping(dimension);
          void focusGridAfterUpdate();
        },
      }));
    }),
  );
  const commandRegistry = $derived([
    ...createCommandRegistry(commandHandlers),
    ...groupingCommands,
  ]);
  const shellCommands = $derived([
    { id: 'go:duplicates', label: 'Possible duplicates', section: 'Go to', run: () => commitNavigation({ workspace: 'relationships', peopleDuplicates: true, identityState: 'candidate' }) },
    ...[...NAVIGATION_ITEMS, SETTINGS_ITEM, ...(['deletions'] as const).map(id => ({ id, label: workspaceLabel(id) }))].map(({ id, label }) => ({ id: `go:${id}`, label, section: 'Go to', run: () => openWorkspaceTab(id) })),
    ...shellData.views.filter(view => !viewIncompatibility(view)).map(view => ({ id: `view:${view.id}`, label: `Open view: ${view.name}`, section: 'Saved views', run: () => void openSavedView(viewState(view)) })),
    { id: 'display', label: 'Display', section: 'Appearance', run: () => { displayOpen = true; } },
    ...(['theme', 'density'] as const).filter(kind => appearance.temporary[kind] !== undefined).map(kind => ({ id: `display:reset-${kind}`, label: `Use Settings ${kind}`, section: 'Appearance', run: () => appearance.clearTemporary(kind) }))
  ]);
  const paletteCommands = $derived(
    [...commandRegistry.map(
      (command): PaletteCommand => ({
        id: command.id,
        label: command.label,
        section: command.section,
        keywords: command.keywords,
        combo: command.combos[0],
        disabled: command.disabled,
      }),
    ), ...shellCommands],
  );
  function runPalette(command: PaletteCommand): void {
    [...commandRegistry, ...shellCommands].find(({ id }) => id === command.id)?.run();
  }
  // KeyboardHelp's search input takes focus before the Modal's trap records
  // where focus came from, so the shell remembers the return target itself.
  function openKeyboardHelp(): void {
    const active = document.activeElement;
    keyboardHelpReturnFocus = active instanceof HTMLElement && active !== document.body ? active : undefined;
    keyboardHelpOpen = true;
  }
  async function closeKeyboardHelp(): Promise<void> {
    keyboardHelpOpen = false;
    await tick();
    const target = keyboardHelpReturnFocus?.isConnected
      ? keyboardHelpReturnFocus
      : (shellRoot?.querySelector<HTMLElement>('button[aria-label="Help"]') ?? drawerOpener());
    keyboardHelpReturnFocus = undefined;
    target?.focus();
  }
  function openRow(row: EntryRow): void {
    // Single-click selects AND opens; re-opening the already-open row must
    // not push a duplicate history entry.
    if (exploreState.current.selectedRow === row.key) return;
    commitNavigation({ selectedRow: row.key });
  }
  function drillGroup(row: ExploreGroupRow): void {
    const [dimension, ...remaining] = exploreState.current.groupingChain;
    if (!dimension || !groupingByDimension(dimension).drillable) return;
    const filters = filtersForGroup(exploreState.current.filters, dimension, row.key);
    if (!filters) {
      commitNavigation({ selectedRow: `group:${dimension}:${row.key}` });
      return;
    }
    commitNavigation({
      filters,
      groupingChain: remaining,
      selectedRow: exploreState.current.workspace === 'files' ? null : `group:${dimension}:${row.key}`,
      activeRow: null,
      scrollAnchor: null,
    });
  }
  $effect(() => {
    keyboardHelpScopeCleanup?.();
    keyboardHelpScopeCleanup = keyboardHelpOpen ? appShortcuts.pushScope('everything-keyboard-help') : undefined;
  });
  // AppShell owns the People controller and opens the URL target on initial
  // load and Back/Forward navigation.
  // Tracked (not untracked) so a predicate change alone — e.g. filters set
  // in Everything/Files that carry over when the URL-carried target stays
  // the same — re-opens the target too, mirroring how Everything's own
  // inspector detail fingerprints include the predicate. Still guarded
  // against re-fetching: the ordinary in-hub click path (which calls
  // onTargetChange and controller.openTarget together, synchronously,
  // before this effect can flush) already opened this exact pair — that
  // guard reads controller.lastPredicateFingerprint (set by openTarget
  // itself) rather than a copy tracked here, so it stays correct no matter
  // which caller opened the target.
  $effect(() => {
    const workspace = exploreState.current.workspace;
    const target = exploreState.current.relationshipTarget;
    if (workspace !== 'relationships') return;
    if (target === null) {
      // URL is truth: once the target is gone (Esc, Back/Forward), drop the
      // hub's detail/timeline state too, so the center pane falls back to
      // its own empty placeholder instead of keeping the previous person or
      // domain on screen.
      if (relationshipsController.target !== null) relationshipsController.clearTarget();
      return;
    }
    const predicate = contextPredicate(exploreState.predicate());
    if (
      relationshipsController.target === target &&
      relationshipsController.lastPredicateFingerprint === predicateFingerprint(predicate)
    ) {
      return;
    }
    void relationshipsController.openTarget(target, predicate);
  });
  function openRelationship(participantID: number): void {
    commitNavigation(relationshipDestination(participantID));
  }
  onMount(() => {
    // Kit popovers close on Escape from a document listener and claim the key with preventDefault
    // so outer layers stay open. The window shortcut listener prevents every key it handles, so
    // note the claim first; this listener must be added before initShortcuts adds that one.
    const noteClaimedEscape = (event: KeyboardEvent): void => {
      claimedEscape = event.key === 'Escape' && event.defaultPrevented ? event : undefined;
    };
    window.addEventListener('keydown', noteClaimedEscape);
    const detachShortcuts = initShortcuts();
    let disposed = false;
    const resyncEditableScope = (): void => {
      if (!disposed) syncEditableShortcutScope(document.activeElement);
    };
    const handleFocusIn = (event: FocusEvent): void => syncEditableShortcutScope(event.target);
    const handleFocusOut = (): void => queueMicrotask(resyncEditableScope);
    const handleHistoryFocus = (): void => {
      // Back/Forward restores committed state synchronously (ExploreState's
      // own popstate listener). A still-pending debounced typing patch must
      // not survive to later clobber that restored state, so it is discarded
      // rather than flushed.
      debouncedSearchPatch.cancel();
      void restoreHistoryFocus();
    };
    const editableObserver = new MutationObserver(() => queueMicrotask(resyncEditableScope));
    document.addEventListener('focusin', handleFocusIn, true);
    document.addEventListener('focusout', handleFocusOut, true);
    document.addEventListener('keydown', preserveNativeControlKey);
    window.addEventListener('popstate', handleHistoryFocus);
    editableObserver.observe(document.documentElement, {
      childList: true,
      subtree: true,
      attributes: true,
      attributeFilter: ['contenteditable'],
    });
    resyncEditableScope();
    const unregister = commandRegistry.flatMap((command) =>
      command.scopes.flatMap((scope) =>
        command.combos.map((combo) => appShortcuts.register(combo, command.run, { scope, description: command.label }))),
    );
    return () => {
      disposed = true;
      document.removeEventListener('focusin', handleFocusIn, true);
      document.removeEventListener('focusout', handleFocusOut, true);
      document.removeEventListener('keydown', preserveNativeControlKey);
      window.removeEventListener('keydown', noteClaimedEscape);
      window.removeEventListener('popstate', handleHistoryFocus);
      editableObserver.disconnect();
      editableScopeCleanup?.();
      editableScopeCleanup = undefined;
      keyboardHelpScopeCleanup?.();
      keyboardHelpScopeCleanup = undefined;
      for (const remove of unregister.reverse()) remove();
      detachShortcuts();
    };
  });
  onDestroy(() => {
    debouncedSearchPatch.cancel();
    loader.destroy();
    archivedMeeting.cancel();
    selectionPreflightController?.abort();
    editableScopeCleanup?.();
    editableScopeCleanup = undefined;
    keyboardHelpScopeCleanup?.();
    keyboardHelpScopeCleanup = undefined;
    appearance.destroy();
    relationshipsController.destroy();
    directoryController.destroy();
    directoryReviewController.destroy();
    operationsController.destroy();
    void flashViewError(shellData.expireDelete());
    if (ownsState) exploreState.destroy();
  });
</script>

<svelte:window bind:innerWidth={viewportWidth} />

<div class="app-shell" class:app-shell--narrow={narrow} bind:this={shellRoot}>
  <span class="kit-sr-only" role="status" aria-label="Operation status" aria-live="polite">
    {#key operationAnnouncement.key}<span>{operationAnnouncement.message}</span>{/key}
  </span>
  {#if !narrow}
    <AppSidebar
      active={exploreState.current.workspace}
      collapsed={sidebarCollapsed}
      showCollapseToggle
      jobs={shellData.jobs}
      duplicateCount={shellData.duplicateCount}
      views={shellData.views}
      deletionsStaged={shellData.deletionsStaged}
      viewsError={shellData.error}
      onRetryViews={() => void shellData.refresh()}
      onPastRuns={() => openOperations()}
      onOpenJob={openJob}
      onOpenView={(view) => void openSavedView(viewState(view))}
      onRenameView={(view, name) => shellData.renameView(view, name)}
      onDescribeView={(view, description) => shellData.describeView(view, description)}
      onDeleteView={view => void flashViewError(shellData.deleteView(view))}
      onNavigate={openWorkspaceTab}
      onToggleCollapsed={toggleSidebar}
      onOpenHelp={openKeyboardHelp}
    />
  {/if}
  <div class="app-column" inert={drawerOpen}>
    <header class="app-top-bar">
      {#if narrow}
        <span class="app-top-bar__menu" bind:this={drawerOpenerHost}>
          <IconButton ariaLabel="Open navigation" onclick={() => { drawerOpen = true; }}>
            <Menu size={18} aria-hidden="true" />
          </IconButton>
        </span>
      {/if}
      {#if exploreState.current.workspace !== 'relationships' || !exploreState.current.peopleDuplicates}
      <SearchBar
        workspace={exploreState.current.workspace}
        query={exploreState.current.workspace === 'relationships' ? exploreState.current.identityQuery ?? '' : exploreState.current.query}
        mode={exploreState.current.searchMode}
        live={exploreState.current.workspace === 'everything' || exploreState.current.workspace === 'files' || exploreState.current.workspace === 'relationships'}
        compact={narrow}
        bind:inputEl={searchInput}
        onDraft={(query, mode) => exploreState.current.workspace === 'relationships' ? debouncedSearchPatch({ identityQuery: query }) : exploreState.replaceSearchDraft(query, mode)}
        onSubmit={submitGlobalSearch}
        filterLabel={exploreState.current.relationshipFacet === 'domains' ? 'Filter domains' : 'Filter people'}
      />
      {/if}
      <div class="app-top-bar__end" aria-label="Appearance controls">
        <DisplayMenu
          bind:open={displayOpen}
          theme={appearance.current.theme}
          density={appearance.current.density}
          overridden={appearance.current.overridden}
          onThemeChange={(theme) => appearance.setTemporary({ theme })}
          onDensityChange={(density) => appearance.setTemporary({ density })}
          onUseDefaults={() => appearance.clearTemporary()}
          columns={exploreState.current.workspace === 'everything' && exploreState.current.presentation === 'table' && exploreState.current.groupingChain.length === 0 ? exploreState.current.columns : undefined}
          onColumnsChange={(columns) => replaceTransient({ columns })}
          readerPosition={exploreState.current.workspace === 'everything' && resultsWidth >= 960 ? readerPosition.value : undefined}
          onReaderPositionChange={(value) => readerPosition.set(value)}
        />
      </div>
    </header>
    <div class="app-main">
      {#if exploreState.current.workspace === 'settings'}
        {#if settings}{@render settings(
          cardDAVSettingsRequest,
          consumeCardDAVSettingsRequest,
          settingsNavigationTarget,
          exploreState.current.settingsCategory,
          selectSettingsCategory,
          () => commitNavigation({ workspace: 'sources' }),
          openOperationAuthority,
          () => void shellData.jobs.refresh(true),
          operationsController,
          () => directoryController.reloadFirstPage()
        )}{/if}
      {:else if exploreState.current.workspace === 'sources'}
        <SourcesWorkspace bind:this={sourcesPage} {client} onStatusChange={() => void shellData.jobs.refresh(true)} onOpenSettings={(settingsCategory, connection) => { cardDAVSettingsRequest = connection ? { connection, key: ++cardDAVSettingsRequestKey } : undefined; commitNavigation({ workspace: 'settings', settingsCategory, settingsAuthority: '' }); }} onOpenOperations={() => openOperations('source_sync')} />
      {:else if exploreState.current.workspace === 'deletions'}
        <DeletionsWorkspace {client} controller={shellData.deletions} onChanged={() => void shellData.refreshDeletions()} />
      {:else if exploreState.current.workspace === 'relationships'}
        <PeopleWorkspace
          onMessagesTableChange={(table) => personMessages = table}
          onConfigureVisualSearch={openVisualAttachmentSettings}
          sweepConfigured={operationsController.snapshot.statusError ? undefined : operationsController.snapshot.statusLanes.flatMap(lane => lane.kinds).find(kind => kind.kind === 'person_sweep')?.configured}
          reviews={directoryReviewController}
          onDuplicatesChanged={() => void identityChanged()}
          directory={directoryController}
          onOpenCardDAVConflict={openCardDAVConflict}
          onOpenCardDAVSettings={openCardDAVSettings}
          urlState={exploreState.current}
          restorationEpoch={exploreState.restorationEpoch}
          onNavigate={commitNavigation}
          onReplaceNavigation={replaceCommittedNavigation}
          onDraftNavigation={replaceCommittedDraft}
          {client}
          controller={relationshipsController}
          target={exploreState.current.relationshipTarget}
          filesOpen={exploreState.current.relationshipFiles}
          predicate={exploreState.predicate()}
          onFiltersChange={(filters) => commitNavigation({ filters })}
          personFilePresentation={exploreState.current.personFilePresentation}
          personFileDirections={exploreState.current.personFileDirections}
          onShowAllChange={(relationshipShowAll) => commitNavigation({ relationshipShowAll })}
          onFilesToggle={(relationshipFiles) => commitNavigation({ relationshipFiles })}
          onPersonFilePresentationChange={(personFilePresentation) =>
            commitNavigation({
              personFilePresentation,
              activeRow: null,
              selectedRow: null,
              scrollAnchor: null,
            })}
          onPersonFileDirectionsChange={(personFileDirections) =>
            commitNavigation({
              personFileDirections,
              activeRow: null,
              selectedRow: null,
              scrollAnchor: null,
            })}
          onOpenPerson={openRelationship}
          onOpenSettings={(category) => commitNavigation(category ? { workspace: 'settings', settingsCategory: category, settingsAuthority: '' } : tasksSettingsDestination())}
          onOpenDirectoryPerson={openDirectoryPerson}
          onAnnounce={announceOperation}
          onOpenFileItem={openFileItem}
          onOpenFileConversation={openFileConversation}
          onOpenMeeting={(meeting) => void openArchivedMeeting(meeting)}
        />
      {:else if exploreState.current.workspace === 'files'}
        <main class="files-shell" aria-label="Files">
          <PageHeader title="Files">
            {#snippet actions()}<Button surface="outline" label="Save view…" onclick={() => (saveViewOpen = true)} />{/snippet}
          </PageHeader>
          {#snippet filesToolbar(controls: Snippet | undefined)}
          <ContextBar
            {client}
            messages
            extraControls={controls}
            query={exploreState.current.query}
            searchMode={exploreState.current.searchMode}
            filters={exploreState.current.filters}
            groupingChain={exploreState.current.groupingChain}
            countLabel={filesCountLabel}
            extraChips={fileFilterChips(exploreState.current.fileFilenameQuery, exploreState.current.fileMIMEFamilies, () => commitNavigation({ fileFilenameQuery: '', activeRow: null, selectedRow: null, scrollAnchor: null }), () => commitNavigation({ fileMIMEFamilies: [], activeRow: null, selectedRow: null, scrollAnchor: null }))}
            sort={exploreState.current.groupingChain.length > 0 ? undefined : {
              options: FILE_SORT_OPTIONS,
              value: fileSortValue(exploreState.current.fileSort),
              onchange: commitFileSort,
            }}
            presentation="files"
            onPresentationChange={(presentation) => {
              if (presentation === 'files') return;
              commitNavigation({
                workspace: 'everything',
                presentation,
                analysisTarget: null,
                selectedIdentifier: null,
                activeRow: null,
                selectedRow: null,
                conversationAnchor: null,
                scrollAnchor: null,
              });
            }}
            onAddGroup={(dimension) => commitGrouping(dimension)}
            onRemoveGroup={(index) =>
              commitNavigation({
                groupingChain: exploreState.current.groupingChain.filter((_, position) => position !== index),
                activeRow: null,
                scrollAnchor: null,
              })}
            onClearFilters={() => { commitNavigation({ filters: [], fileFilenameQuery: '', fileMIMEFamilies: [], activeRow: null, selectedRow: null, scrollAnchor: null }); fileFiltersReset++; }}
            onFiltersChange={(filters) =>
              commitNavigation({
                filters,
                activeRow: null,
                selectedRow: null,
                scrollAnchor: null,
              })}
            onRemoveQuery={() => commitSearch('', exploreState.current.searchMode)}
            onRemoveFilter={(index) =>
              commitNavigation({
                filters: exploreState.current.filters.filter((_, position) => position !== index),
                activeRow: null,
                selectedRow: null,
                scrollAnchor: null,
              })}
          >
            {#snippet filterExtra()}{#key fileFiltersReset}<FileFilters filenameQuery={exploreState.current.fileFilenameQuery} mimeFamilies={exploreState.current.fileMIMEFamilies} onFilenameQueryChange={(fileFilenameQuery) => debouncedSearchPatch({ fileFilenameQuery, activeRow: null, selectedRow: null, scrollAnchor: null })} onMIMEFamiliesChange={(fileMIMEFamilies) => commitNavigation({ fileMIMEFamilies, activeRow: null, selectedRow: null, scrollAnchor: null })} />{/key}{/snippet}
          </ContextBar>
          {/snippet}
          {#if exploreState.current.groupingChain.length > 0}{@render filesToolbar(undefined)}{/if}
          <span class="kit-sr-only" role="status" aria-label="Sort status" aria-live="polite">{sortNotice}</span>
          {#if exploreState.current.groupingChain.length > 0}
            <GroupTable
              rows={loader.groupRows}
              dimension={exploreState.current.groupingChain[0]!}
              workspaceLabel="Files"
              loading={loader.loading}
              loadingMore={loader.loadingMore}
              hasMore={Boolean(loader.nextCursor)}
              totalCount={loader.result?.totalCount}
              generation={loader.resultGeneration}
              error={loader.error}
              pageError={loader.pageError}
              unavailable={loader.unavailable}
              drillable={groupingByDimension(exploreState.current.groupingChain[0]!).drillable}
              focusedKey={exploreState.current.activeRow}
              inspectedKey={readingTargetKey}
              scrollAnchor={exploreState.current.scrollAnchor}
              restoring={loader.restoring}
              onDrill={drillGroup}
              onInspect={drillGroup}
              onLoadMore={loader.loadMore}
              onLoadThroughEnd={loader.loadThroughEnd}
              onActiveKey={(activeRow) => replaceTransient({ activeRow })}
              onScrollAnchor={(key, offset) => replaceTransient({ scrollAnchor: { key, offset } })}
              onRetry={loader.retry}
            />
          {:else}
            <FilesWorkspace
              onConfigureVisualSearch={openVisualAttachmentSettings}
              bind:submitted={loader.submitted}
              {client}
              embedded
              showHeader={false}
              toolbar={filesToolbar}
              bind:fileCount
              bind:fileCountLoading
              predicate={{ ...exploreState.predicate(), grouping: undefined }}
              sort={exploreState.current.fileSort ?? { field: 'occurred_at', direction: 'desc' }}
              filenameQuery={exploreState.current.fileFilenameQuery}
              mimeFamilies={exploreState.current.fileMIMEFamilies}
              activeKey={exploreState.current.activeRow}
              selectedKey={selectedAttachmentID === undefined ? exploreState.current.selectedRow : null}
              restorationEpoch={exploreState.restorationEpoch}
              onRestorationComplete={(epoch) => {
                exploreState.acknowledgeRestoration(epoch);
                // Like Everything, a restored list without an open item takes keyboard focus,
                // unless the person has already moved focus somewhere.
                if (exploreState.current.selectedRow === null && document.activeElement === document.body) {
                  filesGrid()?.focus();
                }
              }}
              onSortChange={(fileSort: FileSearchSort) =>
                commitNavigation({
                  fileSort,
                  activeRow: null,
                  scrollAnchor: null,
                })}
              onFilenameQueryChange={(fileFilenameQuery) =>
                debouncedSearchPatch({
                  fileFilenameQuery,
                  activeRow: null,
                  selectedRow: null,
                  scrollAnchor: null,
                })}
              onMIMEFamiliesChange={(fileMIMEFamilies) =>
                commitNavigation({
                  fileMIMEFamilies,
                  activeRow: null,
                  selectedRow: null,
                  scrollAnchor: null,
                })}
              onActiveKey={(activeRow) => replaceTransient({ activeRow })}
              onSelectedKey={(selectedRow) =>
                selectedRow ? commitNavigation({ selectedRow }) : replaceCommittedNavigation({ selectedRow: null })}
              onOpenItem={openFileItem}
              onOpenConversation={openFileConversation}
            />
          {/if}
        </main>
      {:else}
        <EverythingWorkspace
          bind:resultsWidth
          {client}
          {exploreState}
          {loader}
          session={everythingSession}
          {selection}
          {enabled}
          {readingTargetKey}
          {conversationAnchorId}
          {sortNotice}
          {searchInput}
          {selectionPreflight}
          meetingSelection={apiSelection}
          exportSelection={() => void exportSelection()}
          onReviewDeletion={openDeletionReview}
          {commitNavigation}
          {commitSearch}
          {commitWorkspace}
          {commitGrouping}
          {fixedSortNotice}
          {focusGrid}
          {openRow}
          onOpenRecording={openRecording}
          {drillGroup}
          closeReadingPane={() => void closeReadingPane()}
          {openRelationship}
          {changeConversationAnchor}
          onOpenMeeting={(meeting) => void openArchivedMeeting(meeting)}
          onSaveView={() => (saveViewOpen = true)}
          onStatusChange={() => void shellData.jobs.refresh(true)}
        />
      {/if}
    </div>
  </div>
  {#if saveViewOpen}
    <SaveViewDialog
      {client}
      state={exploreState.current}
      onSaved={(view) => {
        saveViewOpen = false;
        announceOperation(`Saved view ${view.name}.`);
        void shellData.refresh();
      }}
      onclose={() => {
        saveViewOpen = false;
      }}
    />
  {/if}
  {#if narrow && drawerOpen}
    <NavigationDrawer onclose={() => void closeDrawer()}>
      <AppSidebar
        active={exploreState.current.workspace}
        collapsed={false}
        showCollapseToggle={false}
        jobs={shellData.jobs}
        duplicateCount={shellData.duplicateCount}
        views={shellData.views}
        deletionsStaged={shellData.deletionsStaged}
        viewsError={shellData.error}
        onRetryViews={() => void shellData.refresh()}
        onPastRuns={() => { void closeDrawer().then(() => openOperations()); }}
        onOpenJob={(target) => { void closeDrawer().then(() => openJob(target)); }}
        onOpenView={(view) => { void closeDrawer().then(() => openSavedView(viewState(view))); }}
        onRenameView={(view, name) => shellData.renameView(view, name)}
        onDescribeView={(view, description) => shellData.describeView(view, description)}
        onDeleteView={view => { void closeDrawer().then(() => flashViewError(shellData.deleteView(view))); }}
        onNavigate={(id) => {
          openWorkspaceTab(id);
          void closeDrawer();
        }}
        onToggleCollapsed={() => undefined}
        onOpenHelp={() => {
          void closeDrawer().then(openKeyboardHelp);
        }}
      />
    </NavigationDrawer>
  {/if}
</div>

{#if archiveMeetingID !== undefined && !runHistory}
  <ArchivedMeetingReader {client} message={archivedMeeting.detail?.id === archiveMeetingID ? archivedMeeting.detail : undefined}
    loading={archivedMeeting.loading} error={archivedMeeting.error} predicate={exploreState.predicate()}
    anchorID={conversationAnchorId} onClose={closeArchivedMeeting} onOpenPerson={openRelationship}
    onOpenSettings={() => commitNavigation(tasksSettingsDestination())}
    onReload={() => void archivedMeeting.load(archiveMeetingID!)}
    onOpenMeeting={(meeting) => void openArchivedMeeting(meeting)} onAnchorChange={changeConversationAnchor} />
{:else if archivedMeeting.loading}
  <p class="archive-navigation-status" role="status">Loading archived meeting…</p>
{:else if archivedMeeting.error}
  <p class="archive-navigation-status" role="alert">{archivedMeeting.error}</p>
{/if}

{#if runHistory}
  <RunHistoryDialog bind:this={historyDialog} {client} controller={operationsController} bind:state={() => runHistory!, setRunHistory}
    onclose={closeRunHistory} onNavigate={openOperationAuthority} onAnnounce={announceOperation}
    onConfigure={openVisualAttachmentSettings} />
{/if}
<div bind:this={flashRoot}><FlashBanner /></div>
{#if exploreState.current.workspace === 'relationships' && directoryReviewController.mergeRequired}
  {@const merge = directoryReviewController.mergeRequired}
  {#key merge.candidateID}<PersonBindingConflictModal {client} conflict={merge.conflict}
    onOpenProfile={(id) => { directoryReviewController.mergeRequired = null; openDirectoryPerson(id); }}
    onSuccess={async success => { if (exploreState.current.directoryPersonID === success.result.merge.absorbed_person_id) replaceCommittedNavigation({ directoryPersonID: success.survivor.id }); await reconcileIdentity(); await directoryReviewController.completePersonMerge(merge.candidateID, success); }}
    onClose={() => directoryReviewController.mergeRequired = null} />{/key}
{/if}


<CommandPalette bind:open={paletteOpen} commands={paletteCommands} ariaLabel="Messages commands" onrun={runPalette} />

{#if keyboardHelpOpen}
  <KeyboardHelp
    commands={commandRegistry}
    onclose={() => void closeKeyboardHelp()}
    onOpenCommands={() => { void closeKeyboardHelp().then(() => { paletteOpen = true; }); }}
  />
{/if}

{#if contextualViewerFile}
  <FileViewer
    {client}
    file={contextualViewerFile}
    returnFocus={contextualViewerReturnFocus}
    onClose={() => {
      void closeContextualViewer();
    }}
    onOpenItem={(entryKey) => {
      contextualViewerFile = undefined;
      openFileItem(entryKey);
    }}
    onOpenConversation={(entryKey, messageID, conversationID) => {
      contextualViewerFile = undefined;
      openFileConversation(entryKey, messageID, conversationID);
    }}
  />
{/if}

{#if pendingDeletionReview}
  <DeletionReview {client} selection={pendingDeletionReview} onclose={() => pendingDeletionReview = undefined} onStaged={() => { selection.clear(); void shellData.refreshDeletions(); }} />
{/if}

<style>
  .archive-navigation-status { padding: var(--control-gap) var(--pane-pad-x); color: var(--text-secondary); }

  .app-shell {
    display: flex;
    height: 100vh;
    min-height: 100vh;
    overflow: hidden;
    background: var(--bg-primary);
    color: var(--text-primary);
  }

  .app-column {
    display: flex;
    min-width: 0;
    flex: 1;
    flex-direction: column;
  }

  .app-top-bar {
    display: flex;
    min-height: var(--header-height);
    align-items: center;
    gap: var(--control-gap);
    padding: 0 var(--page-gutter);
    background: var(--bg-surface);
    border-bottom: 1px solid var(--border-default);
  }

  .app-top-bar__menu {
    display: contents;
  }

  .app-top-bar__end {
    display: flex;
    align-items: center;
    gap: var(--control-gap);
    margin-left: auto;
  }

  .app-main {
    display: flex;
    min-height: 0;
    flex: 1;
    flex-direction: column;
    overflow: hidden;
  }

  .files-shell {
    display: flex;
    width: 100%;
    min-height: 0;
    flex: 1;
    flex-direction: column;
    gap: var(--field-gap);
    padding: var(--space-5) var(--page-gutter) var(--field-gap);
  }
</style>
