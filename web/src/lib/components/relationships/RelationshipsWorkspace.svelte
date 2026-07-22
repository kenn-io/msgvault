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
  import { appShortcuts, Button, ROOT_SCOPE } from '@kenn-io/kit-ui';
  import { onDestroy, onMount, tick, untrack } from 'svelte';

  import type { APIClient } from '../../api/client';
  import type { ExplorePredicate, FileMIMEFamily, FileSearchSort } from '../../explore/models';
  import type { RelationshipsController } from '../../relationships/controller.svelte';
  import type { RelationshipFacet, RelationshipTimelineRow } from '../../relationships/models';
  import { debounce } from '../../util/debounce';
  import EmptyState from '../common/EmptyState.svelte';
  import FilesWorkspace from '../files/FilesWorkspace.svelte';
  import SplitPane from '../layout/SplitPane.svelte';
  import ReadingPane, { type ReadingPaneSelection } from '../reader/ReadingPane.svelte';
  import RelationshipHeader from './RelationshipHeader.svelte';
  import RelationshipList from './RelationshipList.svelte';
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
    onFacetChange: (facet: RelationshipFacet) => void;
    onTargetChange: (target: string | null) => void;
    onShowAllChange: (value: boolean) => void;
    onFilesToggle: (value: boolean) => void;
    /** Degraded-state escape hatch: switches the parent workspace to
     * Everything. Not part of the frozen Task 4 Props contract — AppShell
     * (Task 6) wires it to its own workspace-change callback. */
    onOpenEverything?: () => void;
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
    onFacetChange,
    onTargetChange,
    onShowAllChange,
    onFilesToggle,
    onOpenEverything = undefined,
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
  let listPaneElement = $state<HTMLDivElement>();
  let containerWidth = $state(1200);
  // Mirrors controller.query for immediate display: the debounce below only
  // delays *writing* controller.query (and so the search fetch it drives),
  // never the text shown in the search box, matching LinkIdentityDialog's
  // "local state for display, debounced write for the network call" split.
  let queryInput = $state(untrack(() => controller.query));

  // A dedicated instance per component, never shared — LinkIdentityDialog's
  // own debounced search is a separate instance for the same reason; sharing
  // one across components previously caused a cross-component bug.
  const debouncedSetQuery = debounce((value: string) => { controller.query = value; }, QUERY_DEBOUNCE_MS);

  // Flush, not cancel: the controller (owned by AppShell) outlives this
  // component across a workspace round-trip, so a flushed write is not
  // lost — it just sits on controller.query until the hub remounts and its
  // own load effect below fires again on mount, refetching with the typed
  // text intact. Cancelling here would silently drop what the user typed.
  onDestroy(() => debouncedSetQuery.flush());

  function handleQueryChange(value: string): void {
    queryInput = value;
    debouncedSetQuery(value);
  }

  // Stable selectors into panes that never unmount when the reading pane or
  // drawer closes, so focus always lands somewhere real instead of falling
  // to <body> (mirrors AppShell's currentGrid()/document.querySelector
  // pattern rather than needing bind:this on every target).
  const TIMELINE_GRID_SELECTOR = '[role="grid"][aria-label="Relationship activity"]';
  const FILES_GRID_SELECTOR = '[role="grid"][aria-label="Files results"]';
  const LIST_GRID_SELECTOR = '[role="grid"][aria-label="Relationship results"]';
  // Excludes tabindex="-1" on every branch, not just the catch-all, so
  // roving-tabindex widgets (e.g. the facet SegmentedControl) only
  // contribute their one active segment as a stop.
  const DRAWER_FOCUSABLE_SELECTOR = [
    'a[href]:not([tabindex="-1"])',
    'button:not([disabled]):not([tabindex="-1"])',
    'input:not([disabled]):not([tabindex="-1"])',
    'select:not([disabled]):not([tabindex="-1"])',
    'textarea:not([disabled]):not([tabindex="-1"])',
    '[tabindex]:not([tabindex="-1"])'
  ].join(', ');

  const layout = $derived(computeHubLayout(containerWidth));
  const predicateFingerprint = $derived(JSON.stringify(predicate));
  const selectedRowKey = $derived(selection?.kind === 'entry' ? selection.row.key : null);

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

  function contextPredicate(value: ExplorePredicate): ExplorePredicate {
    const { cursor: _cursor, grouping: _grouping, candidate_snapshot_id: _snapshot, ...context } = value;
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
    const wasOpen = selection !== undefined;
    selection = undefined;
    conversationAnchorId = undefined;
    conversationBounds = undefined;
    if (wasOpen) await focusTimelinePane();
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

  // Esc closes the reading pane before it ever clears `target` (see
  // handleEscape below), so an in-component walk-back never reaches here
  // with a pane still open. An EXTERNAL clear — browser Back/Forward, which
  // drives `target` straight to null via the URL without going through
  // handleEscape at all — skips that ordering, so without this the reading
  // pane could keep showing a message from the cluster/domain that just
  // closed underneath it. Plain state reset only, no focus side effect:
  // AppShell owns focus restoration for history navigation already.
  $effect(() => {
    if (target !== null) return;
    if (selection === undefined && conversationAnchorId === undefined && conversationBounds === undefined) return;
    selection = undefined;
    conversationAnchorId = undefined;
    conversationBounds = undefined;
  });

  function drawerFocusables(): HTMLElement[] {
    return listPaneElement ? Array.from(listPaneElement.querySelectorAll<HTMLElement>(DRAWER_FOCUSABLE_SELECTOR)) : [];
  }

  function closeDrawer(): void {
    mobileListOpen = false;
    rootElement?.querySelector<HTMLButtonElement>('.drawer-toggle')?.focus();
  }

  function handleDrawerKeydown(event: KeyboardEvent): void {
    if (event.key === 'Escape') {
      event.preventDefault();
      event.stopPropagation();
      closeDrawer();
      return;
    }
    if (event.key !== 'Tab') return;
    const focusables = drawerFocusables();
    if (focusables.length === 0) return;
    const first = focusables[0]!;
    const last = focusables[focusables.length - 1]!;
    if (event.shiftKey && document.activeElement === first) {
      event.preventDefault();
      last.focus();
    } else if (!event.shiftKey && document.activeElement === last) {
      event.preventDefault();
      first.focus();
    }
  }

  // Moves focus into the drawer's first focusable (the search input) the
  // moment it opens; the corresponding trap/Esc-close lives in
  // handleDrawerKeydown above.
  $effect(() => {
    if (layout === 'narrow' && mobileListOpen) {
      void tick().then(() => drawerFocusables()[0]?.focus());
    }
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
    error={controller.listError}
    degraded={controller.degraded}
    {facet}
    query={queryInput}
    {showAll}
    activeTarget={target}
    onQueryChange={handleQueryChange}
    {onFacetChange}
    {onShowAllChange}
    onSelect={selectListRow}
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
                glyph="conversations"
                label="Select a person or domain"
                hint="Choose someone from the list to see your shared history across mail, chat, and files."
              />
            </div>
          {:else}
            <div class="pane-center-column">
              <RelationshipHeader
                detail={controller.detail}
                loading={controller.timelineLoading}
                {filesOpen}
                {onFilesToggle}
                {client}
                onLinkParticipants={(a, b) => controller.linkParticipants(a, b)}
                onUnlinkParticipants={(a, b) => controller.unlinkParticipants(a, b)}
              />
              {#if filesOpen && filesReady}
                <FilesWorkspace
                  {client}
                  predicate={contextPredicate(predicate)}
                  identityScope={identityScopeFor(target)}
                  sort={fileSort}
                  filenameQuery={fileFilenameQuery}
                  mimeFamilies={fileMIMEFamilies}
                  onSortChange={(value) => (fileSort = value)}
                  onFilenameQueryChange={(value) => (fileFilenameQuery = value)}
                  onMIMEFamiliesChange={(value) => (fileMIMEFamilies = value)}
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
                  selectedKey={selectedRowKey}
                  onRowOpen={openTimelineRow}
                  onLoadMore={() => { void controller.loadMoreTimeline(); }}
                />
              {/if}
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
  <h1 class="kit-sr-only">Relationships</h1>
  {#if layout === 'narrow'}
    <Button
      class="drawer-toggle"
      label="Contacts"
      ariaLabel="Show relationship list"
      ariaExpanded={mobileListOpen}
      onclick={() => (mobileListOpen = !mobileListOpen)}
    />
    <!-- svelte-ignore a11y_no_static_element_interactions -- drawer-mode focus trap/inert host; not a control itself. -->
    <div
      class="pane-list drawer"
      class:drawer-open={mobileListOpen}
      inert={!mobileListOpen}
      bind:this={listPaneElement}
      onkeydown={handleDrawerKeydown}
    >
      {@render listPane()}
    </div>
    {@render centerAndReading()}
  {:else}
    <SplitPane
      ariaLabel="Resize relationship list"
      storageKey="msgvault.relationships.list-pane.size"
      initialSize={300}
      minPrimary={240}
      maxPrimary={440}
    >
      {#snippet primary()}
        <div class="pane-list" bind:this={listPaneElement}>
          {@render listPane()}
        </div>
      {/snippet}
      {#snippet secondary()}
        {@render centerAndReading()}
      {/snippet}
    </SplitPane>
  {/if}
</main>

<style>
  .relationships-hub {
    position: relative;
    display: flex;
    min-height: 0;
    height: 100%;
    background: var(--bg-canvas);
  }

  .pane-list {
    display: flex;
    width: 100%;
    height: 100%;
    flex-direction: column;
    overflow: hidden;
    background: var(--bg-primary);
  }

  /* Machined, draggable pane boundary: the kit handle spans 4px of grab
   * area but paints only a centered hairline, so the rail reads as a single
   * machined edge until hovered/focused, when the accent fills the grip. */
  .relationships-hub :global(.kit-split-resize-handle) {
    background: linear-gradient(
      to right,
      transparent calc(50% - 0.5px),
      var(--border-muted) calc(50% - 0.5px),
      var(--border-muted) calc(50% + 0.5px),
      transparent calc(50% + 0.5px)
    );
  }

  .relationships-hub :global(.kit-split-resize-handle:hover),
  .relationships-hub :global(.kit-split-resize-handle:focus-visible) {
    background: var(--accent-blue);
  }

  .pane-list.drawer {
    position: absolute;
    z-index: var(--z-popover, 20);
    inset: 0 auto 0 0;
    width: 280px;
    transform: translateX(-100%);
    background: var(--bg-surface);
    border-right: 1px solid var(--border-default);
    box-shadow: var(--shadow-md);
    transition: transform var(--transition-fast);
  }

  .pane-list.drawer.drawer-open {
    transform: translateX(0);
  }

  .pane-center-and-reading {
    display: flex;
    min-width: 0;
    height: 100%;
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
  }

  .hub-empty {
    display: flex;
    flex: 1;
    flex-direction: column;
  }

  .pane-reading {
    height: 100%;
    background: var(--bg-surface);
  }
</style>
