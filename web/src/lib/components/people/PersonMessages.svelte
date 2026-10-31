<script lang="ts">
  import { appShortcuts, ROOT_SCOPE } from '@kenn-io/kit-ui';
  import { onDestroy, untrack } from 'svelte';
  import type { APIClient } from '../../api/client';
  import type { TimelineScope } from '../../explore/api';
  import { ExploreLoader } from '../../explore/loader.svelte';
  import type { ExploreFilter, ExploreURLState, ExplorePredicate } from '../../explore/models';
  import { ExploreState, ExploreSelectionState } from '../../explore/state.svelte';
  import EverythingWorkspace from '../shell/EverythingWorkspace.svelte';
  import { EverythingSessionState } from '../shell/EverythingSessionState.svelte';
  import { RELATIONSHIP_READER_HISTORY_KEY, relationshipReaderHistory } from '../../relationships/reader-history';
  import type { MeetingRef } from '../../api/generated/models';

  let { client, identityScope, target, predicate, period = null, onOpenPerson, onOpenMeeting, onOpenFiles, onOpenSettings, onClearPeriod, onFiltersChange }:
    { client: APIClient; identityScope: TimelineScope; target: string; predicate: ExplorePredicate; period?: { after: string; before: string } | null;
      onOpenPerson?: (id: number) => void; onOpenMeeting?: (meeting: MeetingRef) => void;
      onOpenFiles: () => void; onFiltersChange: (filters: ExploreFilter[]) => void; onClearPeriod: () => void; onOpenSettings?: () => void } = $props();
  const scope: ExploreFilter = untrack(() => identityScope.kind === 'person'
    ? { dimension: 'participant', values: [String(identityScope.id)] }
    : { dimension: 'domain', values: [identityScope.domain] });
  const exploreState = new ExploreState(null);
  exploreState.replaceCommittedNavigation({ filters: untrack(() => predicate.filters ?? []) });
  const restored = untrack(() => relationshipReaderHistory(window.history.state, 'relationships', target));
  if (restored?.selection.kind === 'entry') exploreState.replaceTransient({ selectedRow: restored.selection.row.key, conversationAnchor: restored.anchor ? String(restored.anchor) : null });
  const selection = new ExploreSelectionState();
  const session = new EverythingSessionState();
  let notice = $state('');
  let root = $state<HTMLElement>();
  const loader = new ExploreLoader(untrack(() => client), exploreState, {
    isEnabled: () => true,
    onPredicateChange: () => selection.clear(),
    onPagingNotice: (value) => notice = value ?? '',
    onRestorationFocus: focusGrid,
  }, untrack(() => identityScope));
  $effect(() => {
    const key = exploreState.current.selectedRow;
    const row = loader.rows.find((row) => row.key === key);
    const anchor = exploreState.current.conversationAnchor;
    if (key && !row) return;
    window.history.replaceState({ ...window.history.state, [RELATIONSHIP_READER_HISTORY_KEY]: row
      ? { target, selection: { kind: 'entry', row: $state.snapshot(row) }, anchor: anchor ? Number(anchor) : undefined }
      : undefined }, '');
  });
  function escape(event: KeyboardEvent): void {
    if (event.key !== 'Escape' || !exploreState.current.selectedRow || appShortcuts.activeScope() !== ROOT_SCOPE) return;
    if ((event.target as Element | null)?.closest('input, textarea, select, [contenteditable]')) return;
    navigate({ selectedRow: null });
    focusGrid();
    event.preventDefault();
    event.stopPropagation();
  }
  function focusGrid(): void { root?.querySelector<HTMLElement>('[role="grid"]')?.focus(); }
  function navigate(patch: Partial<ExploreURLState>): void {
    if (patch.workspace === 'files' || patch.presentation === 'files') { onOpenFiles(); return; }
    if (patch.filters) { onClearPeriod(); onFiltersChange(patch.filters); }
    exploreState.commitNavigation(patch);
  }
  let appliedFilterKey = untrack(() => JSON.stringify({ filters: predicate.filters ?? [], period }));
  const filterKey = $derived(JSON.stringify(predicate.filters ?? []));
  $effect(() => {
    const filters: ExploreFilter[] = JSON.parse(filterKey);
    const window = period;
    const key = JSON.stringify({ filters, period: window });
    if (key === appliedFilterKey) return;
    appliedFilterKey = key;
    untrack(() => exploreState.replaceCommittedNavigation({ filters: window
      ? [...filters.filter((filter) => !['after', 'before'].includes(filter.dimension)),
        { dimension: 'after', values: [window.after] }, { dimension: 'before', values: [window.before] }]
      : filters, selectedRow: null, activeRow: null, scrollAnchor: null }));
  });
  onDestroy(() => { loader.destroy(); exploreState.destroy(); });
</script>

<!-- svelte-ignore a11y_no_static_element_interactions -- Escape closes the embedded reader. -->
<div class="messages" bind:this={root} onkeydown={escape}>
  <EverythingWorkspace {client} scope={scope} {exploreState} {loader} {session} {selection} enabled
    readingTargetKey={exploreState.current.selectedRow} conversationAnchorId={exploreState.current.conversationAnchor ? Number(exploreState.current.conversationAnchor) : undefined}
    sortNotice={notice} searchInput={undefined} selectionPreflight={undefined} meetingSelection={undefined}
    exportSelection={() => undefined} onReviewDeletion={() => undefined} commitNavigation={navigate}
    commitSearch={(query, mode) => exploreState.commitSearch(query, mode)} commitWorkspace={() => onOpenSettings?.()}
    commitGrouping={() => undefined} fixedSortNotice={() => notice = 'Messages are ordered newest first.'} {focusGrid}
    openRow={(row) => navigate({ selectedRow: row.key })} onOpenRecording={(id) => navigate({ selectedRow: `recording:${id}` })}
    drillGroup={() => undefined} closeReadingPane={() => navigate({ selectedRow: null })}
    openRelationship={(id) => onOpenPerson?.(id)} changeConversationAnchor={(id) => exploreState.replaceTransient({ conversationAnchor: String(id) })}
    {onOpenMeeting} onSaveView={() => undefined} />
</div>

<style>
  .messages { display: flex; min-width: 0; min-height: 0; height: 100%; flex: 1; }
</style>
