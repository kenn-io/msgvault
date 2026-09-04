<script lang="ts">
  import {
    Button,
    CommandPalette,
    getThemeMode,
    SelectDropdown,
    StatusDot,
    ThemeToggle,
    TopBar,
    appShortcuts,
    initShortcuts,
    type PaletteCommand
  } from '@kenn-io/kit-ui';
  import { onDestroy, onMount, tick, type Snippet, untrack } from 'svelte';

  import type { APIClient } from '../../api/client';
  import type { components } from '../../api/generated/schema';
  import type {
    EntryRow,
    ExploreColumn,
    ExploreGroupDimension,
    ExploreGroupRow,
    ExploreFileFact,
    ExploreSearchMode,
    ExploreURLState,
    ExploreWorkspace,
    FileViewerTarget,
    FileSearchSort
  } from '../../explore/models';
  import { attachmentSelection, parseAttachmentSelection } from '../../explore/attachment-authority';
  import { filtersForGroup, parseGroupSelection } from '../../explore/group-context';
  import { ExploreLoader } from '../../explore/loader.svelte';
  import { GROUPING_CATALOG, groupingByDimension } from '../../grouping/catalog';
  import { canonicalFingerprint, predicateFingerprint } from '../../explore/selection';
  import { ExploreSelectionState, ExploreState } from '../../explore/state.svelte';
  import { RelationshipsController } from '../../relationships/controller.svelte';
  import { DirectoryController } from '../../directory/controller.svelte';
  import { DirectoryReviewController } from '../../directory/review-controller.svelte';
  import { RelationshipReviewController } from '../../directory/relationship-review-controller.svelte';
  import { FactLedgerController } from '../../directory/fact-ledger-controller.svelte';
  import type { CardDAVSettingsRequest } from '../../carddav/navigation';
  import { createCommandRegistry, type AppCommand, type CommandHandlers } from '../../commands/registry';
  import {
    createAppearancePreferences,
    type AppearanceDefaults,
    type DensityPreference,
    type ThemePreference
  } from '../../theme/preferences.svelte';
  import ContextBar from '../explore/ContextBar.svelte';
  import GroupTable from '../explore/GroupTable.svelte';
  import SavedViewsWorkspace from '../saved-views/SavedViewsWorkspace.svelte';
  import SourcesWorkspace from '../sources/SourcesWorkspace.svelte';
  import DeletionsWorkspace from '../deletions/DeletionsWorkspace.svelte';
  import FilesWorkspace from '../files/FilesWorkspace.svelte';
  import FileViewer from '../files/FileViewer.svelte';
  import RelationshipsWorkspace from '../relationships/RelationshipsWorkspace.svelte';
  import DirectoryWorkspace from '../directory/DirectoryWorkspace.svelte';
  import DirectoryReviewWorkspace from '../directory/DirectoryReviewWorkspace.svelte';
  import KeyboardHelp from './KeyboardHelp.svelte';
  import EverythingWorkspace from './EverythingWorkspace.svelte';
  import { EverythingSessionState } from './EverythingSessionState.svelte';
  import { bufferedCallback } from '../../util/buffered-callback';

  interface Props {
    client: APIClient;
    state?: ExploreState;
    enabled?: boolean;
    settings?: Snippet<[CardDAVSettingsRequest | undefined, (key: number) => void]>;
    appearanceDefaults?: AppearanceDefaults;
    searchModeDefault?: ExploreSearchMode;
  }

  let {
    client,
    state: providedState = undefined,
    enabled = true,
    settings = undefined,
    appearanceDefaults = { theme: 'system', density: 'compact' },
    searchModeDefault = undefined
  }: Props = $props();

  const ownsState = untrack(() => providedState === undefined);
  const exploreState = untrack(() => providedState ?? new ExploreState());
  const ATTACHMENT_HISTORY_MARKER = 'msgvaultAttachmentViewer';
  const DEFAULT_SORT_NOTICE = 'Newest first is the canonical Everything order.';

  const SEARCH_TYPING_DEBOUNCE_MS = 250;
  const debouncedSearchPatch = bufferedCallback(
    (patch: Partial<ExploreURLState>) => {
      // Typing a search query is itself a user-initiated interaction, even
      // though it only ever writes committed *draft* state (never a
      // commit* wrapper) — see the disarming note below. Firing on its own
      // 250ms after the last keystroke, with no navigation ever committing,
      // must disarm the one-shot landing fallback exactly like beforeCommit
      // does; only beforeCommit's own explicit flush of THIS callback (a
      // navigation committing while the patch is still pending) reached it
      // before.
      arrivedWithoutExploreParam = false;
      exploreState.replaceCommittedDraft(patch);
    },
    SEARCH_TYPING_DEBOUNCE_MS
  );

  // A pending debounced search patch (People/Domains identity search, Files
  // filename search) applies to whatever state is current when it eventually
  // fires. If a navigation commits while the patch is still pending, flushing
  // it first applies the typed text immediately so it is not silently
  // dropped; the navigation patch is then committed on top and wins for the
  // fields it touches. Every write to ExploreState funnels through these
  // wrappers so the pending patch is flushed before any navigation commits.
  // This shared path is also where the one-shot landing fallback (see
  // `arrivedWithoutExploreParam` below) gets invalidated: any user-initiated
  // navigation past the initial landing means a later explicit visit to the
  // Relationships hub must show its own degraded state, not bounce away.
  function beforeCommit(): void {
    debouncedSearchPatch.flush();
    arrivedWithoutExploreParam = false;
  }

  // Transient replaces (active row, scroll anchor) are user navigation too:
  // apply any pending typed search patch first so it cannot fire later and
  // clobber the newer transient state with the null active row / scroll
  // anchor it snapshotted at typing time. Unlike beforeCommit this does not
  // disarm the landing fallback, because transient replaces also fire
  // programmatically (e.g. the first loaded row auto-activating); a flush of
  // a pending patch still disarms it inside the debounced callback itself.
  function replaceTransient(patch: Partial<ExploreURLState>): void {
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
    directoryPromotionParticipantID = undefined;
    exploreState.commitWorkspace(workspace);
  }

  function openDirectoryFromRelationship(participantID: number): void {
    beforeCommit();
    directoryController.resetForPromotion();
    directoryPromotionParticipantID = participantID;
    exploreState.commitNavigation({ workspace: 'directory', directoryPersonID: null });
  }

  function openDirectoryPerson(personID: number): void {
    beforeCommit();
    directoryPromotionParticipantID = undefined;
    exploreState.commitNavigation({ workspace: 'directory', directoryPersonID: personID });
  }

  function announceOperation(message: string): void {
    operationAnnouncement = { key: ++operationAnnouncementKey, message };
  }

  function openCardDAVConflict(conflictID: number): void {
    if (!Number.isSafeInteger(conflictID) || conflictID <= 0) return;
    cardDAVSettingsRequest = { conflictID, key: ++cardDAVSettingsRequestKey };
    announceOperation(`Opening CardDAV conflict ${conflictID} in Settings.`);
    commitWorkspace('settings');
  }

  function openCardDAVSettings(): void {
    cardDAVSettingsRequest = { key: ++cardDAVSettingsRequestKey };
    announceOperation('Opening CardDAV settings.');
    commitWorkspace('settings');
  }

  function consumeCardDAVSettingsRequest(key: number): void {
    if (cardDAVSettingsRequest?.key === key) cardDAVSettingsRequest = undefined;
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
    () => Intl.DateTimeFormat().resolvedOptions().timeZone
  );
  // Directory owns ephemeral request/page state while ExploreState remains
  // the browser-restorable source for its query, filters, and selection.
  // Keeping the commit callback at the shell boundary gives it the same
  // history treatment as every other workspace navigation.
  const directoryController = new DirectoryController(
    untrack(() => client),
    (patch, history) => (history === 'replace' ? replaceCommittedDraft(patch) : commitNavigation(patch))
  );
  const directoryReviewController = new DirectoryReviewController(
    untrack(() => client),
    (patch) => commitNavigation(patch)
  );
  const relationshipReviewController = new RelationshipReviewController(
    untrack(() => client),
    (patch) => commitNavigation(patch)
  );
  const factLedgerController = new FactLedgerController(untrack(() => client));
  // Participant IDs only enter here from a successfully loaded
  // /participants/{id} Relationship detail. It is intentionally ephemeral:
  // browser restoration and ordinary Directory navigation never invent a
  // promotion context.
  let directoryPromotionParticipantID = $state<number>();
  let cardDAVSettingsRequest = $state<CardDAVSettingsRequest>();
  let cardDAVSettingsRequestKey = 0;
  let operationAnnouncementKey = 0;
  let operationAnnouncement = $state({ key: 0, message: '' });
  type APIExploreSelection = components['schemas']['ExploreSelection'];
  type ExplorePreflight = components['schemas']['ExplorePreflightResponse'];
  const tabs = [
    { id: 'relationships', label: 'Relationships' },
    { id: 'directory', label: 'Directory' },
    { id: 'directory_review', label: 'Reviews' },
    { id: 'everything', label: 'Everything' },
    { id: 'files', label: 'Files' },
    { id: 'saved_views', label: 'Saved Views' },
    { id: 'sources', label: 'Sources' },
    { id: 'deletions', label: 'Deletions' },
    { id: 'settings', label: 'Settings' }
  ];
  const densityOptions = [
    { value: 'daemon', label: 'Density: Auto' },
    { value: 'compact', label: 'Density: Compact' },
    { value: 'comfortable', label: 'Density: Comfortable' }
  ];

  $effect(() => {
    if (exploreState.current.workspace !== 'settings') cardDAVSettingsRequest = undefined;
  });
  // A default landing (no `explore` param at all — the very first visit,
  // not a URL that named a workspace) starts on the Relationships hub. If
  // the analytical engine turns out to be unavailable, that default silently
  // steps down to Everything instead of leaving a first-time visitor on a
  // hub that can't rank anything. An explicit URL naming (or renaming, via
  // the legacy people/domains rewrite) relationships is a deliberate choice
  // and keeps showing the hub's own degraded state instead.
  //
  // This is a one-shot allowance for the INITIAL landing only: it is
  // invalidated (set false) on the first user-initiated navigation — any
  // commit* wrapper call (see `beforeCommit` above) or a Back/Forward
  // popstate (see `handleHistoryFocus` below) — so a user who lands by
  // default, navigates elsewhere, then explicitly clicks back into
  // Relationships later is never silently bounced away again.
  let arrivedWithoutExploreParam = untrack(
    () => new URLSearchParams(window.location.search).get('explore') === null
  );
  let landingFallbackApplied = false;
  let contextualViewerFile = $state<FileViewerTarget>();
  let contextualViewerReturnFocus = $state<HTMLElement>();
  let previousAttachmentID: number | undefined;
  let selectionPreflight = $state<ExplorePreflight>();
  let selectionPreflightController: AbortController | undefined;
  let pendingDeletionReview = $state<'explicit' | 'all_matching'>();
  let searchInput = $state<HTMLInputElement>();
  // The loader also drives the Files-shell grouped view (AppShell gates its
  // internal load effect on `workspace === 'files' && groupingChain.length >
  // 0` in addition to `workspace === 'everything'`), so it is owned here
  // rather than by EverythingWorkspace alone. `sortNotice`, `selection`, and
  // grid focus restoration are shared shell-wide state the loader cannot own
  // itself; it reaches them only through these callbacks, called at the
  // exact points the original inline effect wrote to that shared state.
  const loader = new ExploreLoader(untrack(() => client), exploreState, {
    isEnabled: () => enabled,
    onPredicateChange: () => selection.clear(),
    onPagingNotice: (message) => { sortNotice = message ?? DEFAULT_SORT_NOTICE; },
    onRestorationFocus: () => focusGrid()
  });
  // Owned here (not by EverythingWorkspace) so coverage-poll backoff, the
  // exact lexical match-count cache, and loaded reading-pane group detail
  // survive a workspace round-trip: AppShell renders EverythingWorkspace
  // behind an {#if}, so it is destroyed and recreated on every switch away
  // from and back to 'everything'.
  const everythingSession = new EverythingSessionState();
  let paletteOpen = $state(false);
  let keyboardHelpOpen = $state(false);
  let keyboardHelpScopeCleanup: (() => void) | undefined;
  let sortNotice = $state(DEFAULT_SORT_NOTICE);
  let editableScopeCleanup: (() => void) | undefined;
  let previousSortNoticeWorkspace = untrack(() => exploreState.current.workspace);
  // An epoch is a monotonically increasing restoration event, not a boolean
  // mode. Capture the initial value so ordinary URL commits do not turn into
  // repeated page-one reloads merely because the initial epoch is positive.
  let appliedDirectoryRestorationEpoch = untrack(() => exploreState.restorationEpoch);
  let appliedDirectoryReviewRestorationEpoch = untrack(() => exploreState.restorationEpoch);
  let appliedFactLedgerRestorationEpoch = untrack(() => exploreState.restorationEpoch);

  $effect(() => {
    const defaults = appearanceDefaults;
    untrack(() => appearance.setDefaults(defaults));
  });

  // URL restoration is shell-owned, just like Relationships' target
  // hydration below. DirectoryWorkspace also supports isolated mounting for
  // component tests, but normal app navigation reaches the controller here.
  $effect(() => {
    const restorationEpoch = exploreState.restorationEpoch;
    const historyRestoration = restorationEpoch !== appliedDirectoryRestorationEpoch;
    appliedDirectoryRestorationEpoch = restorationEpoch;
    if (exploreState.current.workspace !== 'directory') return;
    const directoryState = {
      directoryQuery: exploreState.current.directoryQuery,
      directoryContactState: exploreState.current.directoryContactState,
      directoryCategory: exploreState.current.directoryCategory,
      directoryOrganization: exploreState.current.directoryOrganization,
      directoryPrimaryChannel: exploreState.current.directoryPrimaryChannel,
      directoryLastContactAfter: exploreState.current.directoryLastContactAfter,
      directoryLastContactBefore: exploreState.current.directoryLastContactBefore,
      directorySort: exploreState.current.directorySort,
      directoryPersonID: exploreState.current.directoryPersonID
    };
    untrack(() => directoryController.applyURLState(directoryState, historyRestoration));
  });

  // Review offsets are ephemeral like Directory cursors. A Back/Forward
  // restoration always starts its restored queue at page zero, while an
  // ordinary workspace round trip keeps the AppShell-owned request/page.
  $effect(() => {
    const restorationEpoch = exploreState.restorationEpoch;
    const historyRestoration = restorationEpoch !== appliedDirectoryReviewRestorationEpoch;
    appliedDirectoryReviewRestorationEpoch = restorationEpoch;
    const relationshipState = exploreState.current.relationshipReviewState;
    const factActive = exploreState.current.workspace === 'directory_review' && exploreState.current.reviewKind === 'fact';
    const factHistoryRestoration = factActive && restorationEpoch !== appliedFactLedgerRestorationEpoch;
    if (factActive) appliedFactLedgerRestorationEpoch = restorationEpoch;
    untrack(() => factLedgerController.applyContext(
      factActive,
      exploreState.current.directoryPersonID,
      factHistoryRestoration
    ));
    if (exploreState.current.workspace !== 'directory_review') {
      untrack(() => relationshipReviewController.applyContext(false, relationshipState, historyRestoration));
      return;
    }
    const reviewState = {
      reviewKind: exploreState.current.reviewKind,
      identityState: exploreState.current.identityState
    };
    untrack(() => directoryReviewController.applyURLState(reviewState, historyRestoration));
    untrack(() => relationshipReviewController.applyContext(
      reviewState.reviewKind === 'relationship', relationshipState, historyRestoration
    ));
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
  const readingTargetKey = $derived(
    selectedAttachmentID === undefined ? exploreState.current.selectedRow : null
  );

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
        ...(authority.candidateSnapshotId
          ? { candidate_snapshot_id: authority.candidateSnapshotId }
          : {})
      };
    }
    return {
      mode: 'all_matching',
      predicate: snapshot.predicate,
      exclusions: snapshot.exclusions,
      cache_revision: snapshot.cacheRevision,
      search_provenance: snapshot.searchProvenance,
      ...(snapshot.candidateSnapshotId
        ? { candidate_snapshot_id: snapshot.candidateSnapshotId }
        : {})
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
    void client.POST('/api/v1/explore/preflight', {
      body: { selection: candidate },
      signal: controller.signal
    }).then(({ data }) => {
      if (!controller.signal.aborted) selectionPreflight = data;
    }).catch(() => undefined);
  });

  async function exportSelection(): Promise<void> {
    const target = selectionPreflight?.action_targets.find((item) => item.action === 'export');
    if (!target || !apiSelection || selectionPreflight?.count !== 1) return;
    try {
      const { data, response } = await client.GET('/api/v1/cli/message/raw', {
        params: { query: { id: String(target.message_id) } },
        parseAs: 'blob'
      });
      if (!response.ok || !(data instanceof Blob)) throw new Error('The authorized raw message export is no longer available.');
      const objectURL = URL.createObjectURL(data);
      const anchor = document.createElement('a');
      anchor.href = objectURL;
      anchor.download = target.filename;
      anchor.click();
      URL.revokeObjectURL(objectURL);
    } catch (cause) {
      loader.error = cause instanceof Error ? cause.message : 'Unable to export the selected message.';
    }
  }

  function openDeletionReview(mode: 'explicit' | 'all_matching'): void {
    const candidate = apiSelection;
    if (exploreState.current.workspace !== 'everything' || !candidate || candidate.mode !== mode) return;
    pendingDeletionReview = mode;
    commitWorkspace('deletions');
  }

  async function openSavedView(state: Partial<ExploreURLState>): Promise<void> {
    selection.clear();
    replaceCommittedNavigation(state);
    await tick();
    const grid = currentGrid();
    if (grid) grid.focus();
    else searchInput?.focus();
  }

  function viewerTargetFromFact(file: ExploreFileFact): FileViewerTarget {
    return {
      id: file.id,
      key: file.key,
      entry_key: file.entry_key,
      message_id: file.message_id,
      conversation_id: file.conversation_id,
      filename: file.filename,
      mime_type: file.mime_type,
      size_bytes: file.size
    };
  }

  $effect(() => {
    const attachmentID = selectedAttachmentID;
    const facts = loader.fileFacts;
    if (attachmentID === undefined) {
      contextualViewerFile = undefined;
      if (previousAttachmentID !== undefined) {
        void tick().then(() => (contextualViewerReturnFocus ?? currentGrid())?.focus());
      }
      previousAttachmentID = undefined;
      return;
    }
    previousAttachmentID = attachmentID;
    if (!untrack(() => contextualViewerReturnFocus)) {
      contextualViewerReturnFocus = currentGrid() ?? undefined;
    }
    const local = facts.find((file) => file.id === attachmentID);
    const existing = untrack(() => contextualViewerFile);
    if (local) {
      if (existing?.key !== local.key) contextualViewerFile = viewerTargetFromFact(local);
    } else if (existing?.id !== attachmentID) contextualViewerFile = { id: attachmentID };
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
      element?.closest('input, textarea, select, [contenteditable]:not([contenteditable="false"]), iframe')
    );
  }

  function preserveNativeControlKey(event: KeyboardEvent): void {
    if (!(event.target instanceof Element)) return;
    const target = event.target;
    const activationControl = target.closest('button, a[href], summary, [role="button"], [role="option"]');
    const directionalControl = target.closest('[role="radio"], [role="option"], select');
    if ((activationControl && (event.key === 'Enter' || event.key === ' ')) ||
      (directionalControl && event.key.startsWith('Arrow'))) {
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
    editableScopeCleanup = focused
      ? appShortcuts.pushScope('everything-editable')
      : undefined;
  }

  function focusGrid(): void {
    currentGrid()?.focus();
  }

  async function restoreHistoryFocus(): Promise<void> {
    await tick();
    if (exploreState.current.workspace === 'everything') {
      if (exploreState.current.selectedRow === null) focusGrid();
      return;
    }
    document.querySelector<HTMLButtonElement>('button[aria-current="page"]')?.focus();
  }

  async function focusGridAfterUpdate(): Promise<void> {
    await tick();
    focusGrid();
  }

  function currentGrid(): HTMLElement | null {
    return document.querySelector<HTMLElement>(
      '[role="grid"][aria-label="Everything results"], [role="grid"][aria-label^="Everything grouped by"], [role="grid"][aria-label="Files in current context"]'
    );
  }

  function relayGridKey(event: KeyboardEvent, key: string): void {
    if (event.target instanceof Element && event.target.closest('button, a, summary, [role="button"]')) return;
    const grid = currentGrid();
    if (!grid || event.target === grid) return;
    grid.focus();
    grid.dispatchEvent(new KeyboardEvent('keydown', { key, bubbles: false, cancelable: true }));
  }

  async function closeReadingPane(): Promise<void> {
    commitNavigation({ selectedRow: null });
    await tick();
    focusGrid();
  }

  function openFileItem(entryKey: string): void {
    if (selectedAttachmentID !== undefined) {
      replaceCommittedRestorableNavigation({
        workspace: 'everything', presentation: 'table', selectedRow: entryKey, conversationAnchor: null,
        activeRow: null, scrollAnchor: null
      });
      return;
    }
    commitRestorableNavigation({
      workspace: 'everything', presentation: 'table', selectedRow: entryKey, conversationAnchor: null,
      activeRow: null, scrollAnchor: null
    });
  }

  function openFileConversation(entryKey: string, messageID: number, _conversationID: number): void {
    if (selectedAttachmentID !== undefined) {
      replaceCommittedRestorableNavigation({
        workspace: 'everything', presentation: 'table', selectedRow: entryKey,
        conversationAnchor: String(messageID),
        activeRow: null, scrollAnchor: null
      });
      return;
    }
    commitRestorableNavigation({
      workspace: 'everything', presentation: 'table', selectedRow: entryKey, conversationAnchor: String(messageID),
      activeRow: null, scrollAnchor: null
    });
  }

  function openContextualFile(file: ExploreFileFact): void {
    contextualViewerReturnFocus = currentGrid() ?? undefined;
    contextualViewerFile = viewerTargetFromFact(file);
    commitNavigation({
      selectedRow: attachmentSelection(file.id),
      conversationAnchor: null
    });
    window.history.replaceState(
      { ...(window.history.state && typeof window.history.state === 'object' ? window.history.state : {}),
        [ATTACHMENT_HISTORY_MARKER]: file.id },
      '', window.location.href
    );
  }

  async function closeContextualViewer(): Promise<void> {
    if (window.history.state?.[ATTACHMENT_HISTORY_MARKER] === selectedAttachmentID) {
      window.history.back();
      return;
    }
    replaceCommittedNavigation({ selectedRow: null });
    contextualViewerFile = undefined;
    await tick();
    (contextualViewerReturnFocus ?? currentGrid())?.focus();
  }

  function changeConversationAnchor(anchorId: number): void {
    replaceCommittedNavigation({ conversationAnchor: String(anchorId) });
  }

  // The Relationships hub owns its own Esc layering (reading pane → timeline
  // → list) and only lets an Esc it didn't consume bubble here once it has
  // nothing left to close. This function's branches read/write state that
  // belongs to Everything/Files (selectedRow, groupingChain, the conversation
  // and attachment viewers) — none of it relevant to the hub — so a bubbled
  // Esc must not fall through into clearing leftover state from a workspace
  // the user isn't even in (e.g. a groupingChain left behind by
  // commitWorkspace, which does not reset it).
  function handleEscape(event: KeyboardEvent): void {
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
    const selector = kind === 'filters'
      ? 'button[aria-label="Filters"]'
      : kind === 'grouping'
        ? '[data-group-picker] button'
        : 'button[aria-label="Sort: newest first"]';
    const control = document.querySelector<HTMLButtonElement>(selector);
    control?.focus();
    control?.click();
  }

  function fixedSortNotice(): void {
    sortNotice = 'Everything remains newest first; reverse order is not supported by the canonical entry API.';
    document.querySelector<HTMLButtonElement>('button[aria-label="Sort: newest first"]')?.focus();
  }

  function navigateReader(delta: number): void {
    if (!exploreState.current.selectedRow || loader.rows.length === 0) return;
    const index = loader.rows.findIndex((row) => row.key === exploreState.current.selectedRow);
    if (index < 0) return;
    const next = loader.rows[Math.max(0, Math.min(loader.rows.length - 1, index + delta))];
    if (next && next.key !== exploreState.current.selectedRow) openRow(next);
  }

  function relay(event: KeyboardEvent | undefined, key: string | undefined = undefined): void {
    const resolvedKey = key ?? event?.key;
    if (!resolvedKey) return;
    if (event) {
      relayGridKey(event, resolvedKey);
      return;
    }
    queueMicrotask(() => {
      const grid = currentGrid();
      if (!grid) return;
      grid.focus();
      grid.dispatchEvent(new KeyboardEvent('keydown', { key: resolvedKey, bubbles: false, cancelable: true }));
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
    'focus-search': (event) => { if (!editableTarget(event?.target ?? null)) searchInput?.focus(); },
    'toggle-selection': (event) => relay(event, ' '),
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
    'open-keyboard-help': () => { keyboardHelpOpen = true; },
    'open-command-palette': (event) => { if (!editableTarget(event?.target ?? null)) paletteOpen = true; }
  };
  const groupingCommands = $derived(GROUPING_CATALOG.flatMap((entry) => {
    if (!entry.requestable) return [{
      id: `unavailable:${entry.concept}`,
      label: `${entry.label} — unavailable: ${entry.unavailableReason}`,
      section: 'Group by',
      keywords: `${entry.keywords} ${entry.unavailableReason ?? ''}`,
      keys: [],
      combos: [],
      destructive: false,
      review: false,
      disabled: true,
      run: () => undefined
    }];
    return entry.requestDimensions.map((dimension) => ({
      id: `group:${dimension}`,
      label: `Group by ${dimension === 'year' ? 'Year' : dimension === 'month' ? 'Month' : entry.label}`,
      section: 'Group by',
      keywords: entry.keywords,
      keys: [],
      combos: [],
      destructive: false,
      review: false,
      disabled: exploreState.current.groupingChain.includes(dimension),
      run: () => {
        commitGrouping(dimension);
        void focusGridAfterUpdate();
      }
    }));
  }));
  const reviewWorkspaceCommand: AppCommand = {
    id: 'workspace:directory-review',
    label: 'Open Reviews',
    section: 'Navigate',
    keywords: 'Navigate Reviews Directory identity matches fact review',
    keys: [],
    combos: [],
    destructive: false,
    review: false,
    run: () => commitWorkspace('directory_review')
  };
  const commandRegistry = $derived([
    ...createCommandRegistry(commandHandlers),
    reviewWorkspaceCommand,
    ...groupingCommands
  ]);
  const paletteCommands = $derived(commandRegistry.map((command): PaletteCommand => ({
    id: command.id,
    label: command.label,
    section: command.section,
    keywords: command.keywords,
    combo: command.combos[0],
    disabled: command.disabled
  })));

  function runPalette(command: PaletteCommand): void {
    commandRegistry.find(({ id }) => id === command.id)?.run();
  }

  function applyTemporaryDensity(value: string): void {
    if (value === 'daemon') appearance.clearTemporary('density');
    else appearance.setTemporary({ density: value as DensityPreference });
  }

  function openRow(row: EntryRow): void {
    // Single-click selects AND opens; re-opening the already-open row must
    // not push a duplicate history entry.
    if (exploreState.current.selectedRow === row.key) return;
    commitNavigation({ selectedRow: row.key });
  }

  function drillGroup(row: ExploreGroupRow): void {
    const [dimension, ...remaining] = exploreState.current.groupingChain;
    if (!dimension) return;
    const filters = filtersForGroup(exploreState.current.filters, dimension, row.key);
    if (!filters) {
      commitNavigation({ selectedRow: `group:${dimension}:${row.key}` });
      return;
    }
    commitNavigation({
      filters,
      groupingChain: remaining,
      selectedRow: exploreState.current.workspace === 'files'
        ? null
        : `group:${dimension}:${row.key}`,
      activeRow: null,
      scrollAnchor: null
    });
  }

  $effect(() => {
    keyboardHelpScopeCleanup?.();
    keyboardHelpScopeCleanup = keyboardHelpOpen
      ? appShortcuts.pushScope('everything-keyboard-help')
      : undefined;
  });

  // Hydrates the Relationships hub's detail pane from a URL-carried target
  // (initial load, or Back/Forward restoring a different one). The hub
  // itself never opens a target on its own — RelationshipsWorkspace's own
  // tests drive controller.openTarget explicitly — so AppShell, as the
  // controller's owner, is the one place that reacts to the URL field.
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
    const predicate = exploreState.predicate();
    if (relationshipsController.target === target &&
      relationshipsController.lastPredicateFingerprint === predicateFingerprint(predicate)) {
      return;
    }
    void relationshipsController.openTarget(target, predicate);
  });

  $effect(() => {
    if (landingFallbackApplied || !arrivedWithoutExploreParam) return;
    if (exploreState.current.workspace !== 'relationships') return;
    if (!relationshipsController.degraded || relationshipsController.degraded.readiness === 'building') return;
    landingFallbackApplied = true;
    // A committed replace, not a transient one: `committed` is what the
    // next push rewrites the current history entry from (see
    // ExploreState.navigate's 'push' branch). Leaving `committed` behind at
    // the degraded 'relationships' landing would mean the very next push
    // silently rewrites this entry back to a state the user never actually
    // saw, so Back would return to the degraded hub instead of wherever
    // they actually came from.
    exploreState.replaceCommittedNavigation({ workspace: 'everything' });
  });

  function openRelationship(participantID: number): void {
    commitNavigation({
      workspace: 'relationships',
      // Entering the hub never carries the text query (see
      // ExploreState.commitWorkspace): the relationships ranking and
      // cluster-timeline endpoints have no text-query input, so a carried
      // query would half-apply across the hub's surfaces.
      query: '',
      relationshipFacet: 'people',
      relationshipTarget: `cluster:${participantID}`,
      relationshipShowAll: false,
      analysisTarget: null,
      selectedIdentifier: null,
      selectedRow: null,
      activeRow: null,
      conversationAnchor: null,
      scrollAnchor: null
    });
  }

  onMount(() => {
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
      // A Back/Forward navigation is user-initiated, same as any commit*
      // wrapper call — it ends the one-shot landing-fallback allowance (see
      // `arrivedWithoutExploreParam` above).
      arrivedWithoutExploreParam = false;
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
      attributeFilter: ['contenteditable']
    });
    resyncEditableScope();
    const unregister = commandRegistry.flatMap((command) => command.combos.map((combo) =>
      appShortcuts.register(combo, command.run, { description: command.label })
    ));
    return () => {
      disposed = true;
      document.removeEventListener('focusin', handleFocusIn, true);
      document.removeEventListener('focusout', handleFocusOut, true);
      document.removeEventListener('keydown', preserveNativeControlKey);
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
    selectionPreflightController?.abort();
    editableScopeCleanup?.();
    editableScopeCleanup = undefined;
    keyboardHelpScopeCleanup?.();
    keyboardHelpScopeCleanup = undefined;
    appearance.destroy();
    relationshipsController.destroy();
    directoryController.destroy();
    directoryReviewController.destroy();
    relationshipReviewController.destroy();
    factLedgerController.destroy();
    if (ownsState) exploreState.destroy();
  });
</script>

<div class="app-shell">
  <span class="kit-sr-only" role="status" aria-label="Operation status" aria-live="polite">
    {#key operationAnnouncement.key}<span>{operationAnnouncement.message}</span>{/key}
  </span>
  <TopBar
    {tabs}
    active={exploreState.current.workspace}
    centerTabs
    onchange={(workspace) => commitWorkspace(workspace as ExploreWorkspace)}
  >
    {#snippet left()}
      <div class="brand" aria-label="msgvault home"><span aria-hidden="true">◇</span> msgvault</div>
    {/snippet}
    {#snippet right()}
      <div class="appearance-controls" aria-label="Appearance controls">
        <ThemeToggle />
        {#if appearance.temporary.theme !== undefined}
          <Button
            size="sm"
            surface="soft"
            label="Use daemon theme"
            onclick={() => appearance.clearTemporary('theme')}
          />
        {/if}
        <SelectDropdown
          title="Temporary density"
          value={appearance.temporary.density ?? 'daemon'}
          options={densityOptions}
          align="end"
          onchange={applyTemporaryDensity}
        />
      </div>
      <span class="archive-state" class:archive-state--error={Boolean(loader.error || loader.unavailable)}>
        <span aria-hidden="true">
          <StatusDot
            status={loader.loading ? 'working' : loader.error || loader.unavailable ? 'unclean' : 'idle'}
            label={loader.loading ? 'Searching' : loader.error || loader.unavailable ? 'Archive needs attention' : 'Local archive ready'}
          />
        </span>
        {loader.loading ? 'Searching' : loader.error || loader.unavailable ? 'Attention' : 'Local archive'}
      </span>
    {/snippet}
  </TopBar>

  {#if exploreState.current.workspace === 'settings'}
    {#if settings}{@render settings(cardDAVSettingsRequest, consumeCardDAVSettingsRequest)}{/if}
  {:else if exploreState.current.workspace === 'saved_views'}
    <SavedViewsWorkspace
      {client}
      currentState={exploreState.current}
      selection={selection.snapshot()}
      onOpen={(state) => { void openSavedView(state); }}
    />
  {:else if exploreState.current.workspace === 'sources'}
    <SourcesWorkspace {client} />
  {:else if exploreState.current.workspace === 'deletions'}
    <DeletionsWorkspace
      {client}
      selection={apiSelection}
      reviewOnMount={pendingDeletionReview === apiSelection?.mode}
      onReviewStarted={() => { pendingDeletionReview = undefined; }}
    />
  {:else if exploreState.current.workspace === 'relationships'}
    <RelationshipsWorkspace
      {client}
      controller={relationshipsController}
      facet={exploreState.current.relationshipFacet}
      target={exploreState.current.relationshipTarget}
      showAll={exploreState.current.relationshipShowAll}
      filesOpen={exploreState.current.relationshipFiles}
      predicate={exploreState.predicate()}
      personFilePresentation={exploreState.current.personFilePresentation}
      personFileDirections={exploreState.current.personFileDirections}
      onFacetChange={(relationshipFacet) => commitNavigation({ relationshipFacet })}
      onTargetChange={(relationshipTarget) => commitNavigation({ relationshipTarget })}
      onShowAllChange={(relationshipShowAll) => commitNavigation({ relationshipShowAll })}
      onFilesToggle={(relationshipFiles) => commitNavigation({ relationshipFiles })}
      onPersonFilePresentationChange={(personFilePresentation) => commitNavigation({
        personFilePresentation, activeRow: null, selectedRow: null, scrollAnchor: null
      })}
      onPersonFileDirectionsChange={(personFileDirections) => commitNavigation({
        personFileDirections, activeRow: null, selectedRow: null, scrollAnchor: null
      })}
      onOpenEverything={() => commitWorkspace('everything')}
      onOpenDirectory={openDirectoryFromRelationship}
      onOpenDirectoryPerson={openDirectoryPerson}
      onAnnounce={announceOperation}
      onOpenFileItem={openFileItem}
      onOpenFileConversation={openFileConversation}
    />
  {:else if exploreState.current.workspace === 'directory'}
    <DirectoryWorkspace
      {client}
      controller={directoryController}
      promotionParticipantID={directoryPromotionParticipantID}
      state={{
        directoryQuery: exploreState.current.directoryQuery,
        directoryContactState: exploreState.current.directoryContactState,
        directoryCategory: exploreState.current.directoryCategory,
        directoryOrganization: exploreState.current.directoryOrganization,
        directoryPrimaryChannel: exploreState.current.directoryPrimaryChannel,
        directoryLastContactAfter: exploreState.current.directoryLastContactAfter,
        directoryLastContactBefore: exploreState.current.directoryLastContactBefore,
        directorySort: exploreState.current.directorySort,
        directoryPersonID: exploreState.current.directoryPersonID
      }}
      onOpenCardDAVConflict={openCardDAVConflict}
      onOpenCardDAVSettings={openCardDAVSettings}
      onAnnounce={announceOperation}
    />
  {:else if exploreState.current.workspace === 'directory_review'}
    <DirectoryReviewWorkspace
      controller={directoryReviewController}
      relationshipController={relationshipReviewController}
      factController={factLedgerController}
      directoryPersonID={exploreState.current.directoryPersonID}
      onOpenDirectory={() => commitWorkspace('directory')}
      onOpenPerson={openDirectoryPerson}
      onAnnounce={announceOperation}
    />
  {:else if exploreState.current.workspace === 'files'}
    <div class="files-shell">
      <ContextBar
        {client}
        query={exploreState.current.query}
        searchMode={exploreState.current.searchMode}
        filters={exploreState.current.filters}
        groupingChain={exploreState.current.groupingChain}
        totalCount={exploreState.current.groupingChain.length > 0 ? loader.result?.totalCount : undefined}
        presentation="files"
        onPresentationChange={(presentation) => {
          if (presentation === 'files') return;
          commitNavigation({
            workspace: 'everything', presentation, analysisTarget: null, selectedIdentifier: null,
            activeRow: null, selectedRow: null, conversationAnchor: null, scrollAnchor: null
          });
        }}
        onAddGroup={(dimension) => commitGrouping(dimension)}
        onRemoveGroup={(index) => commitNavigation({
          groupingChain: exploreState.current.groupingChain.filter((_, position) => position !== index),
          activeRow: null,
          scrollAnchor: null
        })}
        onClearFilters={() => commitNavigation({ filters: [], activeRow: null, scrollAnchor: null })}
        onFiltersChange={(filters) => commitNavigation({
          filters, activeRow: null, selectedRow: null, scrollAnchor: null
        })}
      />
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
          drillable={groupingByDimension(exploreState.current.groupingChain[0]!).requestable}
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
          {client}
          predicate={{ ...exploreState.predicate(), grouping: undefined }}
          sort={exploreState.current.fileSort ?? { field: 'occurred_at', direction: 'desc' }}
          filenameQuery={exploreState.current.fileFilenameQuery}
          mimeFamilies={exploreState.current.fileMIMEFamilies}
          activeKey={exploreState.current.activeRow}
          selectedKey={exploreState.current.selectedRow}
          restorationEpoch={exploreState.restorationEpoch}
          onRestorationComplete={(epoch) => {
            exploreState.acknowledgeRestoration(epoch);
          }}
          onSortChange={(fileSort: FileSearchSort) => commitNavigation({
            fileSort, activeRow: null, scrollAnchor: null
          })}
          onFilenameQueryChange={(fileFilenameQuery) => debouncedSearchPatch({
            fileFilenameQuery, activeRow: null, selectedRow: null, scrollAnchor: null
          })}
          onMIMEFamiliesChange={(fileMIMEFamilies) => commitNavigation({
            fileMIMEFamilies, activeRow: null, selectedRow: null, scrollAnchor: null
          })}
          onActiveKey={(activeRow) => replaceTransient({ activeRow })}
          onSelectedKey={(selectedRow) => selectedRow
            ? commitNavigation({ selectedRow })
            : replaceCommittedNavigation({ selectedRow: null })}
          onOpenItem={openFileItem}
          onOpenConversation={openFileConversation}
        />
      {/if}
    </div>
  {:else}
    <EverythingWorkspace
      {client}
      {exploreState}
      {loader}
      session={everythingSession}
      {selection}
      {enabled}
      {readingTargetKey}
      {conversationAnchorId}
      {sortNotice}
      bind:searchInput
      {selectionPreflight}
      exportSelection={() => void exportSelection()}
      {commitNavigation}
      {commitWorkspace}
      {commitGrouping}
      {commitSearch}
      {fixedSortNotice}
      {focusGrid}
      {openRow}
      {drillGroup}
      {openFileItem}
      {openContextualFile}
      closeReadingPane={() => void closeReadingPane()}
      {openRelationship}
      {changeConversationAnchor}
    />
  {/if}
</div>

<CommandPalette
  bind:open={paletteOpen}
  commands={paletteCommands}
  ariaLabel="Everything commands"
  onrun={runPalette}
/>

{#if keyboardHelpOpen}
  <KeyboardHelp commands={commandRegistry} onclose={() => { keyboardHelpOpen = false; }} />
{/if}

{#if contextualViewerFile}
  <FileViewer
    {client}
    file={contextualViewerFile}
    returnFocus={contextualViewerReturnFocus}
    onClose={() => { void closeContextualViewer(); }}
    onOpenItem={(entryKey) => { contextualViewerFile = undefined; openFileItem(entryKey); }}
    onOpenConversation={(entryKey, messageID, conversationID) => {
      contextualViewerFile = undefined;
      openFileConversation(entryKey, messageID, conversationID);
    }}
  />
{/if}

<style>
  .app-shell {
    display: flex;
    min-width: 0;
    min-height: 100vh;
    height: 100vh;
    flex-direction: column;
    overflow: hidden;
    background: var(--bg-primary);
    color: var(--text-primary);
  }

  .brand {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
    color: var(--text-primary);
    font-family: var(--font-sans);
    font-size: var(--font-size-md);
    font-weight: 650;
    letter-spacing: 0.01em;
  }

  .brand span {
    color: var(--artifact-ink);
    font-size: var(--font-size-sm);
  }

  /* Machined app-bar boundary: darker hairline plus a faint sheen line. */
  .app-shell :global(.kit-top-bar) {
    box-shadow: 0 1px 0 var(--hairline-sheen);
  }

  /* Integrated app-bar tabs: quiet text buttons with a soft active pill
   * instead of kit-ui's detached inset track. */
  .app-shell :global(.kit-top-bar__tabs) {
    gap: var(--space-1);
    padding: 0;
    background: transparent;
    border-radius: 0;
  }

  .app-shell :global(.kit-top-bar__tab) {
    padding: 5px 12px;
    border-radius: var(--radius-md);
    font-size: var(--font-size-md);
  }

  .app-shell :global(.kit-top-bar__tab.active) {
    background: var(--bg-subtle);
    box-shadow: none;
  }

  .archive-state {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
    margin-left: var(--space-3);
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    white-space: nowrap;
  }

  .appearance-controls {
    display: inline-flex;
    gap: var(--space-2);
  }

  .files-shell {
    display: flex;
    width: 100%;
    max-width: 1760px;
    min-height: 0;
    flex: 1;
    flex-direction: column;
    gap: var(--space-4);
    margin-inline: auto;
    padding: var(--space-6) var(--space-7) var(--space-4);
  }
</style>
