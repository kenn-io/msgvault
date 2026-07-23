<script lang="ts">
  import { Button, KbdBadge, SearchInput } from '@kenn-io/kit-ui';
  import { onDestroy, untrack } from 'svelte';

  import type { APIClient } from '../../api/client';
  import type { components } from '../../api/generated/schema';
  import type {
    EntryRow,
    AllMatchingExploreSelection,
    ExploreCacheUnavailable,
    ExploreColumn,
    ExploreFileFact,
    ExploreGroupDimension,
    ExploreGroupRow,
    ExploreSearchMode,
    ExploreURLState,
    ExploreWorkspace
  } from '../../explore/models';
  import { createExploreAPI } from '../../explore/api';
  import { filtersForGroup, parseGroupSelection } from '../../explore/group-context';
  import { findGroupDetail } from '../../explore/group-detail';
  import type { ExploreLoader } from '../../explore/loader.svelte';
  import { groupingByDimension } from '../../grouping/catalog';
  import { canonicalFingerprint, createAllMatchingSelection, predicateFingerprint } from '../../explore/selection';
  import type { ExploreSelectionState, ExploreState } from '../../explore/state.svelte';
  import ContextBar from '../explore/ContextBar.svelte';
  import EverythingTable from '../explore/EverythingTable.svelte';
  import FilesPresentation from '../explore/FilesPresentation.svelte';
  import GroupTable from '../explore/GroupTable.svelte';
  import SelectionBar from '../explore/SelectionBar.svelte';
  import SplitPane from '../layout/SplitPane.svelte';
  import PersonTimeline from '../people/PersonTimeline.svelte';
  import SearchCoverage from '../search/SearchCoverage.svelte';
  import SearchModeControl from '../search/SearchModeControl.svelte';
  import ReadingPane, {
    type ReadingPaneSelection,
    type ReadingPaneStatus
  } from '../reader/ReadingPane.svelte';
  import type { SearchCoverageAction } from '../../search/modes';
  import type { EverythingSessionState } from './EverythingSessionState.svelte';

  type ExplorePreflight = components['schemas']['ExplorePreflightResponse'];

  interface Props {
    client: APIClient;
    exploreState: ExploreState;
    loader: ExploreLoader;
    session: EverythingSessionState;
    selection: ExploreSelectionState;
    enabled: boolean;
    readingTargetKey: string | null;
    conversationAnchorId: number | undefined;
    sortNotice: string;
    searchInput?: HTMLInputElement;
    selectionPreflight: ExplorePreflight | undefined;
    exportSelection: () => void;
    commitNavigation: (patch: Partial<ExploreURLState>) => void;
    commitWorkspace: (workspace: ExploreWorkspace) => void;
    commitGrouping: (dimension: ExploreGroupDimension) => void;
    commitSearch: (query: string, mode: ExploreSearchMode) => void;
    fixedSortNotice: () => void;
    focusGrid: () => void;
    openRow: (row: EntryRow) => void;
    drillGroup: (row: ExploreGroupRow) => void;
    openFileItem: (entryKey: string) => void;
    openContextualFile: (file: ExploreFileFact) => void;
    closeReadingPane: () => void;
    openRelationship: (participantID: number) => void;
    changeConversationAnchor: (anchorId: number) => void;
  }

  let {
    client,
    exploreState,
    loader,
    session,
    selection,
    enabled,
    readingTargetKey,
    conversationAnchorId,
    sortNotice,
    searchInput = $bindable(undefined),
    selectionPreflight,
    exportSelection,
    commitNavigation,
    commitWorkspace,
    commitGrouping,
    commitSearch,
    fixedSortNotice,
    focusGrid,
    openRow,
    drillGroup,
    openFileItem,
    openContextualFile,
    closeReadingPane,
    openRelationship,
    changeConversationAnchor
  }: Props = $props();

  const api = createExploreAPI(untrack(() => client));

  // coverage, coveragePollAttempts/coveragePollKey, visibleLexicalRowKeys,
  // lexicalCountCache/canonicalQueryHashes, and readingGroupDetail/
  // readingDetailGeneration/readingDetailFingerprint live on `session`
  // (owned by AppShell) so they survive this component being destroyed and
  // recreated on a workspace round-trip. Timer handles, AbortControllers,
  // and other in-flight-request bookkeeping below stay local: they are
  // cleaned up in `onDestroy` and re-armed fresh on the next mount.
  let coverageRetryRevision = $state(0);
  let coveragePollTimer: ReturnType<typeof setTimeout> | undefined;
  let coverageRequestGeneration = 0;
  let coverageController: AbortController | undefined;
  let lexicalCountController: AbortController | undefined;
  let lexicalCountTimer: ReturnType<typeof setTimeout> | undefined;
  let lexicalCountRequestKey = '';
  let readingDetailLoading = $state(false);
  let readingDetailError = $state('');
  let readingDetailUnavailable = $state<ExploreCacheUnavailable>();
  let readingDetailController: AbortController | undefined;

  const allMatchingSelection = $derived.by((): AllMatchingExploreSelection | undefined => {
    if (!loader.result || loader.loading || loader.loadingMore) return undefined;
    const predicate = exploreState.predicate();
    if (exploreState.current.presentation !== 'table' || exploreState.current.groupingChain.length > 0) return undefined;
    if (loader.resultFingerprint !== predicateFingerprint(predicate)) return undefined;
    // requestGeneration and resultGeneration are always equal while this
    // workspace is mounted: the loader syncs them together at request
    // start, and only its early-return branch (a different workspace)
    // advances one without the other.
    return createAllMatchingSelection(predicate, loader.result, loader.resultGeneration);
  });

  const readingSelection = $derived.by((): ReadingPaneSelection | undefined => {
    const selected = readingTargetKey;
    if (!selected) return undefined;
    const entry = loader.rows.find((row) => row.key === selected);
    if (entry) return { kind: 'entry', row: entry };
    const group = parseGroupSelection(selected);
    return group && session.readingGroupDetail?.kind === 'group' &&
      session.readingGroupDetail.dimension === group.dimension && session.readingGroupDetail.key === group.key
      ? session.readingGroupDetail
      : undefined;
  });

  const readingState = $derived.by((): {
    status: ReadingPaneStatus;
    message: string;
    unavailable?: ExploreCacheUnavailable;
  } => {
    const selected = readingTargetKey;
    if (!selected || readingSelection) return { status: 'ready', message: '' };
    if (parseGroupSelection(selected)) {
      if (readingDetailUnavailable) {
        return {
          status: 'unavailable',
          message: readingDetailUnavailable.message,
          unavailable: readingDetailUnavailable
        };
      }
      if (readingDetailLoading || !readingDetailError) return { status: 'loading', message: '' };
      return {
        status: 'missing',
        message: readingDetailError
      };
    }
    if (loader.unavailable) {
      return { status: 'unavailable', message: loader.unavailable.message, unavailable: loader.unavailable };
    }
    if (loader.loading || loader.restoring) return { status: 'loading', message: '' };
    return {
      status: loader.error ? 'error' : 'missing',
      message: loader.error || 'The selected entry is no longer available in this context.'
    };
  });

  const readingPredicateFingerprint = $derived(predicateFingerprint(exploreState.predicate()));

  $effect(() => {
    const target = parseGroupSelection(exploreState.current.selectedRow);
    const fingerprint = readingPredicateFingerprint;
    // Tracked, not untracked like `predicate` below: a cache rebuild
    // invalidates a stale detail whether it happened before this mount
    // (`session` persisted the fingerprint across a prior workspace
    // round-trip that destroyed and recreated this component) or resolves
    // just after it, while this component was already waiting on it. Gated
    // below on `cacheRevision === undefined` so this settles the reuse
    // decision once per predicate/target rather than firing an extra,
    // wasted request the instant the main list's own load transitions from
    // "revision not yet known" (a fresh mount, before its own list request
    // has resolved even once) to whatever revision it turns out to be.
    const cacheRevision = loader.result?.cacheRevision;
    const predicate = untrack(() => exploreState.predicate());
    if (target && cacheRevision === undefined) {
      readingDetailLoading = true;
      readingDetailError = '';
      readingDetailUnavailable = undefined;
      return;
    }
    const loadedKey = target ? `${fingerprint}|${cacheRevision}|group:${target.dimension}:${target.key}` : '';
    // A workspace round-trip destroys and recreates this component, which
    // re-creates this effect and would otherwise unconditionally discard and
    // refetch the group detail below. If a previous mount already loaded
    // this exact group under this exact predicate — persisted on `session`,
    // which survives the round-trip — reuse it instead. Read through
    // `untrack` so this effect's dependencies stay limited to the selection
    // and predicate (matching the pre-persistence behavior); this effect
    // also writes `session.readingGroupDetail`/`readingDetailFingerprint`
    // below, and a tracked read of the same fields here would make the
    // effect re-trigger itself on every write.
    const canReuse = untrack(() => Boolean(
      target && loadedKey === session.readingDetailFingerprint && session.readingGroupDetail
    ));
    if (canReuse) {
      readingDetailLoading = false;
      readingDetailError = '';
      readingDetailUnavailable = undefined;
      return;
    }
    session.readingDetailGeneration += 1;
    readingDetailController?.abort();
    readingDetailController = undefined;
    session.readingGroupDetail = undefined;
    session.readingDetailFingerprint = '';
    readingDetailError = '';
    readingDetailUnavailable = undefined;
    if (!target) {
      readingDetailLoading = false;
      return;
    }
    const filters = filtersForGroup(predicate.filters ?? [], target.dimension, target.key);
    if (!filters) {
      readingDetailLoading = false;
      readingDetailError = 'The selected group is not available in this context.';
      return;
    }
    const generation = session.readingDetailGeneration;
    const controller = new AbortController();
    readingDetailController = controller;
    readingDetailLoading = true;
    const detailPredicate = {
      ...predicate,
      cursor: undefined,
      candidate_snapshot_id: undefined,
      grouping: undefined,
      filters,
      presentation: 'table' as const
    };
    // Filtering by target.key does not make it the top-ranked group: every
    // co-participant/co-domain of the matching entries forms a group too, so
    // findGroupDetail pages through the listing for the exact key.
    void findGroupDetail(api, detailPredicate, target.dimension, target.key, controller.signal)
      .then((lookup) => {
        if (generation !== session.readingDetailGeneration || controller.signal.aborted) return;
        if (lookup.status === 'unavailable') {
          readingDetailUnavailable = lookup.unavailable;
          return;
        }
        if (lookup.status === 'missing') {
          readingDetailError = 'The selected group is no longer available in this context.';
          return;
        }
        session.readingGroupDetail = {
          kind: 'group',
          dimension: target.dimension,
          key: target.key,
          label: lookup.row.label,
          count: lookup.row.count,
          estimatedBytes: lookup.row.estimated_bytes,
          latestAt: lookup.row.latest_at
        };
        session.readingDetailFingerprint = loadedKey;
      })
      .catch((cause: unknown) => {
        if (generation !== session.readingDetailGeneration || controller.signal.aborted) return;
        readingDetailError = cause instanceof Error ? cause.message : 'Could not restore the selected group.';
      })
      .finally(() => {
        if (generation === session.readingDetailGeneration) readingDetailLoading = false;
      });
  });

  const coverageFiltersFingerprint = $derived(canonicalFingerprint(exploreState.current.filters));

  $effect(() => {
    const workspace = exploreState.current.workspace;
    const mode = exploreState.current.searchMode;
    void coverageRetryRevision;
    void coverageFiltersFingerprint;
    const filters = untrack(() => exploreState.current.filters);
    const pollKey = `${workspace}|${mode}|${coverageFiltersFingerprint}`;
    if (pollKey !== session.coveragePollKey) {
      session.coveragePollKey = pollKey;
      session.coveragePollAttempts = 0;
    }
    if (coveragePollTimer !== undefined) {
      clearTimeout(coveragePollTimer);
      coveragePollTimer = undefined;
    }
    coverageController?.abort();
    const generation = ++coverageRequestGeneration;
    if (!enabled || workspace !== 'everything' || mode === 'full_text') {
      session.coverage = undefined;
      return;
    }
    coverageController = new AbortController();
    const controller = coverageController;
    void api.coverage(filters, controller.signal)
      .then((loaded) => {
        if (generation !== coverageRequestGeneration || controller.signal.aborted) return;
        session.coverage = loaded;
        if (loaded.status === 'initializing') {
          const delay = Math.min(10_000, 1_000 * 2 ** session.coveragePollAttempts);
          session.coveragePollAttempts += 1;
          coveragePollTimer = setTimeout(() => {
            coveragePollTimer = undefined;
            if (generation === coverageRequestGeneration) coverageRetryRevision += 1;
          }, delay);
        } else {
          session.coveragePollAttempts = 0;
        }
      })
      .catch((cause: unknown) => {
        if (generation !== coverageRequestGeneration || controller.signal.aborted) return;
        if (cause instanceof DOMException && cause.name === 'AbortError') return;
        session.coveragePollAttempts = 0;
        session.coverage = {
          eligible_count: session.coverage?.eligible_count ?? 0,
          embedded_count: session.coverage?.embedded_count ?? 0,
          percentage: session.coverage?.percentage ?? 0,
          cache_revision: session.coverage?.cache_revision ?? '',
          status: 'unavailable',
          detail: cause instanceof Error ? cause.message : 'Semantic coverage could not be loaded.',
          actions: ['retry']
        };
      });
  });

  $effect(() => {
    const predicate = exploreState.predicate();
    const currentResult = loader.result;
    const rowKeys = [...session.visibleLexicalRowKeys].sort();
    if (lexicalCountTimer !== undefined) {
      clearTimeout(lexicalCountTimer);
      lexicalCountTimer = undefined;
    }
    if (!currentResult || exploreState.current.presentation === 'files' || !predicate.query ||
      (predicate.search_mode !== 'full_text' && predicate.search_mode !== 'hybrid') ||
      loader.resultFingerprint !== predicateFingerprint(predicate) || rowKeys.length === 0) {
      lexicalCountController?.abort();
      lexicalCountController = undefined;
      lexicalCountRequestKey = '';
      return;
    }
    const lexicalRevision = currentResult.searchProvenance.lexical_index_revision;
    if (!lexicalRevision) return;
    const countPredicateFingerprint = predicateFingerprint(predicate);
    const canonicalHashKey = `${lexicalRevision}\u0000${countPredicateFingerprint}`;
    session.lexicalCountCache.invalidateRevision(currentResult.cacheRevision);
    const canonicalQueryHash = session.canonicalQueryHashes.get(canonicalHashKey);
    const cacheKey = session.lexicalCountCache.key({
      query: predicate.query,
      ...(canonicalQueryHash ? { canonicalQueryHash } : {}),
      cacheRevision: currentResult.cacheRevision,
      lexicalRevision,
      predicateFingerprint: countPredicateFingerprint,
      rowKeys
    });
    const cached = canonicalQueryHash ? session.lexicalCountCache.get(cacheKey) : undefined;
    if (cached) {
      lexicalCountController?.abort();
      lexicalCountController = undefined;
      lexicalCountRequestKey = '';
      applyLexicalCounts(cached);
      return;
    }
    if (lexicalCountRequestKey === cacheKey) return;
    lexicalCountController?.abort();
    lexicalCountController = undefined;
    lexicalCountRequestKey = cacheKey;
    const countPredicate = {
      ...predicate,
      ...(currentResult.candidateSnapshotId
        ? { candidate_snapshot_id: currentResult.candidateSnapshotId }
        : {})
    };
    lexicalCountTimer = setTimeout(() => {
      lexicalCountTimer = undefined;
      const controller = new AbortController();
      lexicalCountController = controller;
      void api.matchCounts(countPredicate, rowKeys, controller.signal)
        .then((loaded) => {
          if (controller.signal.aborted || loaded.cacheRevision !== currentResult.cacheRevision ||
            loaded.lexicalRevision !== lexicalRevision) return;
          if (predicateFingerprint(exploreState.predicate()) !== countPredicateFingerprint) return;
          session.canonicalQueryHashes.delete(canonicalHashKey);
          while (session.canonicalQueryHashes.size >= 128) {
            const oldest = session.canonicalQueryHashes.keys().next().value;
            if (oldest === undefined) break;
            session.canonicalQueryHashes.delete(oldest);
          }
          session.canonicalQueryHashes.set(canonicalHashKey, loaded.canonicalQueryHash);
          const exactKey = session.lexicalCountCache.key({
            query: predicate.query!, canonicalQueryHash: loaded.canonicalQueryHash,
            cacheRevision: loaded.cacheRevision, lexicalRevision: loaded.lexicalRevision,
            predicateFingerprint: countPredicateFingerprint, rowKeys
          });
          session.lexicalCountCache.set(exactKey, loaded.counts, loaded.cacheRevision);
          applyLexicalCounts(loaded.counts);
        })
        .catch((cause: unknown) => {
          if (!(cause instanceof DOMException && cause.name === 'AbortError')) {
            // Exact counts are supplementary. Search results retain their own named state.
          }
        })
        .finally(() => {
          if (lexicalCountController === controller) lexicalCountController = undefined;
          if (lexicalCountRequestKey === cacheKey) lexicalCountRequestKey = '';
        });
    }, 60);
  });

  function applyLexicalCounts(counts: Record<string, number>): void {
    // A persisted cache hit calls this synchronously from inside the
    // lexicalCount $effect below (the miss path only reaches it from an
    // async `.then`, outside that effect's tracked window). Reading
    // `loader.rows` untracked keeps this effect from depending on the very
    // state it writes here, which would otherwise re-trigger itself forever.
    const rows = untrack(() => loader.rows);
    loader.rows = rows.map((row) => counts[row.key] === undefined ? row : {
      ...row,
      match: { ...row.match, lexical_match_count: counts[row.key] }
    });
  }

  async function handleCoverageAction(action: SearchCoverageAction): Promise<void> {
    if (action === 'retry') {
      coverageRetryRevision += 1;
      return;
    }
    try {
      await api.runCoverageAction(action, session.coverage?.status ?? 'unavailable');
      coverageRetryRevision += 1;
    } catch (cause) {
      session.coverage = {
        eligible_count: session.coverage?.eligible_count ?? 0,
        embedded_count: session.coverage?.embedded_count ?? 0,
        percentage: session.coverage?.percentage ?? 0,
        cache_revision: session.coverage?.cache_revision ?? '',
        status: 'unavailable',
        detail: cause instanceof Error ? cause.message : 'The semantic index action failed.',
        actions: ['retry']
      };
    }
  }

  function submitSearch(event: SubmitEvent): void {
    event.preventDefault();
    commitSearch(exploreState.current.query.trim(), exploreState.current.searchMode);
    focusGrid();
  }

  function inspectGroup(row: ExploreGroupRow): void {
    const dimension = exploreState.current.groupingChain[0];
    if (dimension) commitNavigation({ selectedRow: `group:${dimension}:${row.key}` });
  }

  onDestroy(() => {
    coverageRequestGeneration += 1;
    coverageController?.abort();
    if (coveragePollTimer !== undefined) clearTimeout(coveragePollTimer);
    if (lexicalCountTimer !== undefined) clearTimeout(lexicalCountTimer);
    lexicalCountController?.abort();
    session.readingDetailGeneration += 1;
    readingDetailController?.abort();
  });
</script>

<main class="everything-workspace" aria-label="Everything">
  <header class="workspace-header">
    <div>
      <h1>Everything</h1>
    </div>
    <p class="result-count" aria-live="polite" data-mono>
      {#if loader.result?.totalCount !== undefined}
        {loader.result.totalCount.toLocaleString()} items
      {:else if loader.result?.candidatePoolSaturated}
        Candidate pool capped
      {:else}
        Modality-neutral archive
      {/if}
    </p>
  </header>

  <form class="search-bar" role="search" aria-label="Search Everything" onsubmit={submitSearch}>
    <div class="query-control">
      <SearchInput
        id="everything-search"
        bind:inputEl={searchInput}
        value={exploreState.current.query}
        ariaLabel="Search everything"
        placeholder="Search people, conversations, events, and files…"
        block
        oninput={(value) => exploreState.replaceSearchDraft(value, exploreState.current.searchMode)}
      />
    </div>
    <SearchModeControl
      requestedMode={exploreState.current.searchMode}
      status={session.coverage?.status}
      error={loader.error}
      onchange={(mode: ExploreSearchMode) => exploreState.replaceSearchDraft(
        exploreState.current.query,
        mode
      )}
    />
    <Button type="submit" label="Search" tone="info" surface="solid" />
  </form>

  {#if session.coverage}
    <SearchCoverage
      requestedMode={exploreState.current.searchMode}
      coverage={session.coverage}
      onaction={handleCoverageAction}
    />
  {/if}

  <ContextBar
    query={exploreState.current.query}
    searchMode={exploreState.current.searchMode}
    filters={exploreState.current.filters}
    groupingChain={exploreState.current.groupingChain}
    totalCount={loader.result?.totalCount}
    presentation={exploreState.current.presentation}
    onPresentationChange={(presentation) => commitNavigation({
      presentation, activeRow: null, selectedRow: null, scrollAnchor: null
    })}
    onAddGroup={(dimension) => commitGrouping(dimension)}
    onRemoveGroup={(index) => commitNavigation({
      groupingChain: exploreState.current.groupingChain.filter((_, position) => position !== index),
      activeRow: null,
      scrollAnchor: null
    })}
    onClearFilters={() => commitNavigation({ filters: [], activeRow: null, scrollAnchor: null })}
    onSort={fixedSortNotice}
  />
  <span class="kit-sr-only" role="status" aria-label="Sort status" aria-live="polite">{sortNotice}</span>

  {#if loader.result?.searchDeletionScope === 'active'}
    <p class="scope-note" role="status">Semantic search covers active messages only.</p>
  {/if}

  <div class="results-split">
    <SplitPane
      ariaLabel="Resize reading pane"
      storageKey="msgvault.reading-pane.size"
      orientation="vertical"
      initialFraction={0.55}
      minPrimary={120}
      minSecondary={160}
      collapsed={!readingTargetKey}
    >
    {#snippet primary()}
    <div class="results-primary">
    {#if exploreState.current.groupingChain.length > 0}
      <GroupTable
      rows={loader.groupRows}
      dimension={exploreState.current.groupingChain[0]!}
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
      onInspect={inspectGroup}
      onLoadMore={loader.loadMore}
      onLoadThroughEnd={loader.loadThroughEnd}
      onActiveKey={(activeRow) => exploreState.replaceTransient({ activeRow })}
      onScrollAnchor={(key, offset) => exploreState.replaceTransient({ scrollAnchor: { key, offset } })}
      onRetry={loader.retry}
      />
    {:else if exploreState.current.presentation === 'files'}
      <FilesPresentation
        files={loader.fileFacts}
        loading={loader.loading}
        loadingMore={loader.loadingMore}
        hasMore={Boolean(loader.nextCursor)}
        totalCount={loader.result?.totalCount}
        generation={loader.resultGeneration}
        error={loader.error}
        pageError={loader.pageError}
        unavailable={loader.unavailable}
        focusedKey={exploreState.current.activeRow}
        scrollAnchor={exploreState.current.scrollAnchor}
        restoring={loader.restoring}
        onOpenFile={openContextualFile}
        onOpenItem={openFileItem}
        onActiveKey={(activeRow) => exploreState.replaceTransient({ activeRow })}
        onScrollAnchor={(key, offset) => exploreState.replaceTransient({ scrollAnchor: { key, offset } })}
        onLoadMore={loader.loadMore}
        onRetry={loader.retry}
      />
    {:else}
      <SelectionBar
      {selection}
      totalCount={loader.result?.totalCount}
      allMatching={allMatchingSelection}
      preflight={selectionPreflight}
      onExport={exportSelection}
    />
      {#if exploreState.current.presentation === 'timeline'}
        <PersonTimeline
          rows={loader.rows}
          {selection}
          loading={loader.loading}
          loadingMore={loader.loadingMore}
          hasMore={Boolean(loader.nextCursor)}
          totalCount={loader.result?.totalCount}
          generation={loader.resultGeneration}
          unavailable={loader.unavailable}
          error={loader.error}
          pageError={loader.pageError}
          focusedKey={exploreState.current.activeRow}
          inspectedKey={readingTargetKey}
          scrollAnchor={exploreState.current.scrollAnchor}
          restoring={loader.restoring}
          onOpen={openRow}
          onScrollAnchor={(key, offset) => exploreState.replaceTransient({ scrollAnchor: { key, offset } })}
          onLoadMore={loader.loadMore}
          onLoadThroughEnd={loader.loadThroughEnd}
          onActiveKey={(activeRow) => exploreState.replaceTransient({ activeRow })}
          onVisibleRows={(rowKeys) => { session.visibleLexicalRowKeys = rowKeys; }}
          onRetry={loader.retry}
        />
      {:else}
      <EverythingTable
        rows={loader.rows}
        {selection}
        columns={exploreState.current.columns}
        columnWidths={exploreState.current.columnWidths}
        focusedKey={exploreState.current.activeRow}
        inspectedKey={readingTargetKey}
        scrollAnchor={exploreState.current.scrollAnchor}
        restoring={loader.restoring}
        loading={loader.loading}
        loadingMore={loader.loadingMore}
        hasMore={Boolean(loader.nextCursor)}
        totalCount={loader.result?.totalCount}
        generation={loader.resultGeneration}
        unavailable={loader.unavailable}
        error={loader.error}
        pageError={loader.pageError}
        onOpen={openRow}
        onColumnsChange={(columns: ExploreColumn[]) => exploreState.replaceTransient({ columns })}
        onScrollAnchor={(key, offset) => exploreState.replaceTransient({ scrollAnchor: { key, offset } })}
        onLoadMore={loader.loadMore}
        onLoadThroughEnd={loader.loadThroughEnd}
        onActiveKey={(activeRow) => exploreState.replaceTransient({ activeRow })}
        onVisibleRows={(rowKeys) => { session.visibleLexicalRowKeys = rowKeys; }}
        onRetry={loader.retry}
      />
      {/if}
    {/if}
    </div>
    {/snippet}
    {#snippet secondary()}
      {#if readingTargetKey}
        <ReadingPane
          {client}
          selection={readingSelection}
          targetKey={readingTargetKey}
          status={readingState.status}
          statusMessage={readingState.message}
          unavailable={readingState.unavailable}
          predicate={exploreState.predicate()}
          onClose={closeReadingPane}
          onOpenSettings={() => commitWorkspace('settings')}
          onOpenRelationship={openRelationship}
          {conversationAnchorId}
          onConversationAnchorChange={changeConversationAnchor}
        />
      {/if}
    {/snippet}
    </SplitPane>
  </div>

  <footer class="keyboard-help" aria-label="Keyboard shortcuts">
    <span><KbdBadge keys={['J']} />/<KbdBadge keys={['K']} /> move</span>
    <span><KbdBadge keys={['Enter']} /> open</span>
    <span><KbdBadge keys={['Space']} /> select</span>
    <span><KbdBadge keys={['Shift', 'Space']} /> range</span>
    <span><KbdBadge keys={['A']} /> visible</span>
    <span><KbdBadge keys={['X']} /> clear</span>
    <span><KbdBadge keys={['/']} /> search</span>
    <span><KbdBadge keys={['Esc']} /> back</span>
  </footer>
</main>

<style>
  .everything-workspace {
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

  .workspace-header {
    display: flex;
    align-items: baseline;
    justify-content: space-between;
    gap: var(--space-6);
  }

  h1 {
    margin: 0;
    font-family: var(--font-sans);
    font-size: var(--font-size-xl);
    font-weight: 650;
    line-height: 1.2;
  }

  .result-count {
    margin: 0;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    font-variant-numeric: tabular-nums;
  }

  .search-bar {
    display: flex;
    align-items: center;
    gap: var(--space-4);
  }

  .query-control {
    min-width: 240px;
    flex: 1;
  }

  .scope-note {
    margin: 0;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  .results-split {
    display: flex;
    min-width: 0;
    min-height: 200px;
    flex: 1;
    overflow: hidden;
  }

  .results-primary {
    display: flex;
    min-width: 0;
    height: 100%;
    flex-direction: column;
  }

  /* The reading pane provides its own surface; the split's secondary pane
   * only frames it with the hairline above the drag handle. */
  .results-split :global([data-pane='secondary']) {
    border: 1px solid var(--border-default);
    border-top: 0;
    border-radius: 0 0 var(--radius-md) var(--radius-md);
  }

  .keyboard-help {
    display: flex;
    min-height: 22px;
    align-items: center;
    gap: var(--space-5);
    color: var(--text-muted);
    font-size: var(--font-size-2xs);
  }

  @media (max-width: 760px) {
    .everything-workspace {
      padding-inline: var(--space-4);
    }

    .search-bar {
      align-items: stretch;
      flex-wrap: wrap;
    }

    .query-control {
      min-width: 100%;
    }

    .keyboard-help {
      overflow-x: auto;
    }
  }
</style>
