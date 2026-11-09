<script lang="ts">
  import { untrack } from 'svelte';
  import { Button, EmptyState, SegmentedControl } from '@kenn-io/kit-ui';
  import EmptyLine from '../common/EmptyLine.svelte';
  import type { APIClient } from '../../api/client';
  import type { DirectoryController } from '../../directory/controller.svelte';
  import PersonDetails from './PersonDetails.svelte';
  import PersonRecordActions from '../directory/PersonRecordActions.svelte';
  import type { PersonMergeSuccess } from '../../directory/person-merge';
  import type { MessagesTable } from '../../explore/loader.svelte';
  import type { MeetingRef } from '../../api/generated/models';
  import { contextPredicate, type RelationshipsController, type RelationshipsMergeContext } from '../../relationships/controller.svelte';
  import type { DomainSummary, PersonSummary, ExplorePredicate, FileSearchSort, FileMIMEFamily, PersonFileDirection, ExploreFilter } from '../../explore/models';
  import PersonAddresses from './PersonAddresses.svelte';
  import { formatContactDate } from '../../directory/labels';
  import MergePeopleDialog from './MergePeopleDialog.svelte';
  import RelationshipCalendar from '../relationships/RelationshipCalendar.svelte';
  import { localDayBoundsUTC } from '../relationships/timeline-support';
  import { RELATIONSHIP_READER_HISTORY_KEY, relationshipReaderHistory } from '../../relationships/reader-history';
  import FilesWorkspace from '../files/FilesWorkspace.svelte';
  import PersonHeader from './PersonHeader.svelte';
  import InlineNotice from '../common/InlineNotice.svelte';
  import PersonMessages from './PersonMessages.svelte';
  import DomainCalendar from './DomainCalendar.svelte';

  let { client, directory, onRecordDeleted, onSplitCommitted, onOpenCardDAVConflict, onOpenCardDAVSettings, controller, target, filesOpen, predicate, onFilesToggle, onBack, durablePersonID = null,
    onConfigureVisualSearch, onOpenDirectoryPerson, onAnnounce, onOpenPerson, onOpenMeeting, onOpenSettings, sweepConfigured,
    onOpenFileItem, onOpenFileConversation, onReconcilePersonMerge, onIdentityChange, personFilePresentation = 'files', personFileDirections = ['from_person'],
    onPersonFilePresentationChange, onPersonFileDirectionsChange, onFiltersChange, onMessagesTableChange,
    details = $bindable(false), selectedDate = $bindable<string | null>(null), domainYear = $bindable<number | null>(null),
    sort = $bindable<FileSearchSort>({ field: 'occurred_at', direction: 'desc' }), filenameQuery = $bindable(''), mimeFamilies = $bindable<FileMIMEFamily[]>([]) }:
    { client: APIClient; directory: DirectoryController; onRecordDeleted?: (personID: number) => void; onSplitCommitted?: (context: import('../../directory/person-merge-history-controller.svelte').PersonSplitCommittedContext) => void | Promise<void>; onOpenCardDAVConflict?: (id: number) => void; onOpenCardDAVSettings?: () => void; controller: RelationshipsController; target: string | null; filesOpen: boolean; predicate: ExplorePredicate; durablePersonID?: number | null;
      onReconcilePersonMerge: (context: RelationshipsMergeContext, participantsLinked?: boolean, success?: PersonMergeSuccess) => Promise<void>; onIdentityChange?: () => Promise<void>;
      onFilesToggle: (value: boolean) => void; onBack?: () => void;
      onOpenDirectoryPerson?: (id: number) => void; onAnnounce?: (message: string) => void; onOpenPerson?: (id: number) => void;
      sweepConfigured?: boolean; onConfigureVisualSearch?: () => void; onOpenMeeting?: (meeting: MeetingRef) => void; onOpenSettings?: (category?: string) => void; onOpenFileItem?: (key: string) => void;
      onOpenFileConversation?: (key: string, messageID: number, conversationID: number) => void;
      personFilePresentation?: 'media' | 'files'; personFileDirections?: PersonFileDirection[];
      details?: boolean; selectedDate?: string | null; domainYear?: number | null; sort?: FileSearchSort; filenameQuery?: string; mimeFamilies?: FileMIMEFamily[];
      onFiltersChange: (filters: ExploreFilter[]) => void;
      onMessagesTableChange?: (table: MessagesTable | undefined) => void;
      onPersonFilePresentationChange?: (value: 'media' | 'files') => void; onPersonFileDirectionsChange?: (value: PersonFileDirection[]) => void } = $props();
  const context = $derived(contextPredicate(predicate));
  const domain = $derived(target?.startsWith('domain:') ? target.slice(7) : undefined);
  const detail = $derived(controller.target === target ? controller.detail : null);
  const person = $derived(detail && !domain ? detail as PersonSummary : null);
  const scope = $derived(domain ? { kind: 'domain' as const, domain } : controller.target === target && controller.canonicalID !== null ? { kind: 'person' as const, id: controller.canonicalID } : undefined);
  const profileID = $derived(durablePersonID ?? person?.profile?.id ?? null);
  const savedProfile = $derived(directory.selectedPersonID === profileID ? directory.profile : null);
  $effect(() => {
    void savedProfile;
    // Stop following loading state once this profile's unfiltered metadata is available.
    if (untrack(() => !directory.detail)) void directory.detailLoading;
    if (untrack(() => target === null || controller.target !== target || controller.timelineLoading)) void controller.timelineLoading;
    untrack(() => {
      if (directory.detailLoading || directory.selectedPersonID !== profileID || !directory.detail) return;
      if (target === null ? directory.detail.person?.participant_ids.length : controller.target !== target || controller.timelineLoading) return;
      details ||= !filesOpen &&
        !(controller.target === target && target !== null && controller.firstContactAt);
    });
  });
  const name = $derived(domain ?? savedProfile?.person?.display_name ?? person?.display_label ?? 'Person');
  let saving = $state(false);
  let mergeOpen = $state(false);
  let staleLink = $state<{ a: number; b: number }>();
  $effect(() => { void target; staleLink = undefined; mergeOpen = false; });
  function linked(a: number, b: number, result: import('../../relationships/controller.svelte').LinkOutcome): void {
    if (result.ok) staleLink = result.cacheState === 'stale' ? { a, b } : undefined;
  }
  async function retryLink(): Promise<void> {
    if (!staleLink) return;
    const receipt = staleLink, { a, b } = receipt;
    const result = await controller.linkParticipants(a, b);
    if (result.ok && staleLink === receipt) linked(a, b, result);
  }
  async function savePerson(): Promise<void> {
    if (!person || saving) return;
    saving = true;
    try { const result = await directory.promote(person.id); if (result.ok) await controller.reconcilePersonMerge(controller.personMergeContextSnapshot()); } finally { saving = false; }
  }
  const email = $derived(person?.identifiers?.find((identity) => identity.type === 'email')?.value ?? '');
  const since = $derived(controller.firstContactAt ? new Intl.DateTimeFormat('en-US', { month: 'short', year: 'numeric' }).format(new Date(controller.firstContactAt)) : undefined);
  const period = $derived.by(() => {
    if (!selectedDate) return null;
    const bounds = localDayBoundsUTC(`${selectedDate}T00:00:00`);
    return { after: bounds.start, before: bounds.end };
  });
  $effect(() => {
    if (!selectedDate || !detail) return;
    const year = Number(selectedDate.slice(0, 4));
    if (domain) domainYear = year;
    else if (controller.relationshipCalendarYear !== year) void controller.loadRelationshipYear(year);
  });
  // Files pushes its own entry first, so this drops the cleared day only from that entry's reader.
  function clearDate(): void {
    selectedDate = null;
    const reader = relationshipReaderHistory(window.history.state, 'relationships', target);
    if (reader) window.history.replaceState({ ...window.history.state, [RELATIONSHIP_READER_HISTORY_KEY]: { ...reader, bounds: undefined } }, '');
  }
  $effect(() => { if (filesOpen) clearDate(); });
  // Linking or unlinking moves a person's activity, so the open profile and the caller's saved list refresh too.
  async function refreshSaved<T extends { ok: boolean }>(outcome: T): Promise<T> {
    if (outcome.ok) await Promise.all([savedProfile ? directory.refreshSelectedPerson() : undefined, onIdentityChange?.()]);
    return outcome;
  }
</script>

{#snippet files()}
  <div class="files"><FilesWorkspace {onConfigureVisualSearch} {client} embedded showHeader={false} predicate={context} identityScope={profileID === null ? scope : { kind: 'durable-person', id: profileID }} {sort} {filenameQuery} {mimeFamilies}
    personPresentation={personFilePresentation} personDirections={personFileDirections} onSortChange={(value) => sort = value}
    onFilenameQueryChange={(value) => filenameQuery = value} onMIMEFamiliesChange={(value) => mimeFamilies = value}
    onPersonPresentationChange={onPersonFilePresentationChange} onPersonDirectionsChange={onPersonFileDirectionsChange}
    onOpenItem={onOpenFileItem} onOpenConversation={onOpenFileConversation} /></div>
{/snippet}

<section class="person-page" class:details-open={details} role="group" aria-label="Person page">
  {#if staleLink}<div class="summary-error" role="alert">Identity saved; the cache refresh failed.<Button label="Retry" onclick={() => void retryLink()} /></div>{/if}
  {#if durablePersonID !== null || (detail && !controller.detailMissing)}
    <PersonHeader {name} {email} domain={Boolean(domain)} {onBack} onOpenDetails={() => details = true}>
      {#snippet actions()}
        {#if !domain}<Button label="Merge with…" onclick={() => mergeOpen = true} />{/if}
        {#if profileID !== null && savedProfile}{#key savedProfile}<PersonRecordActions {client} controller={savedProfile} personID={profileID} onOpenPerson={onOpenDirectoryPerson} {onAnnounce} onDeleted={onRecordDeleted} {onSplitCommitted} {onOpenCardDAVConflict} {onOpenCardDAVSettings} onSetUpTracking={sweepConfigured === false ? () => onOpenSettings?.('enrichment') : undefined} />{/key}
        {:else if person && profileID === null}<Button label={saving ? 'Saving…' : 'Save'} disabled={saving} onclick={() => void savePerson()} />
        {:else if directory.detailLoading && durablePersonID !== null && !details}<span class="header-status" role="status">Loading details…</span>{/if}
      {/snippet}
    </PersonHeader>
  {/if}
  {#if controller.timelineLoading && !detail && durablePersonID === null}
    <p role="status">Loading…</p>{#if onBack}<Button label="Back to People" onclick={onBack} />{/if}
  {:else if controller.detailMissing && durablePersonID === null}
    <EmptyState title="Person or domain not found" />
    {#if onBack}<Button label="Back to People" onclick={onBack} />{/if}
  {:else if !detail && durablePersonID === null}
    <EmptyState title={controller.timelineError ?? 'Select a person or domain'} />
    {#if onBack}<Button label="Back to People" onclick={onBack} />{/if}
    {#if controller.timelineError && target}<Button label="Retry" onclick={() => target && controller.openTarget(target, predicate)} />{/if}
  {:else}
    {#if directory.promotionError}<InlineNotice message="Couldn't save this person." details={directory.promotionError} actionLabel="Retry" onaction={() => void savePerson()} />{/if}
    {#if controller.timelineError}
      <div class="summary-error" role="alert"><span>{controller.timelineError}</span><Button label="Retry" onclick={() => target && controller.openTarget(target, predicate)} /></div>
    {/if}
    {#if !filesOpen && !details && (controller.headerState === 'normal' || !scope)}
      <div class="calendar">
        {#if domain}
          <DomainCalendar bind:year={domainYear} {client} {domain} revision={detail?.cache_revision ?? ''} firstAt={controller.firstContactAt} lastAt={controller.lastContactAt} {selectedDate} onDateChange={(date) => selectedDate = date} />
        {:else if scope}
          <RelationshipCalendar calendar={controller.relationshipCalendar} loading={controller.relationshipCalendarLoading} error={controller.relationshipCalendarError}
            year={controller.relationshipCalendarYear} firstYear={controller.relationshipCalendarFirstYear} currentYear={controller.relationshipCalendarCurrentYear}
            {selectedDate} {since} title="Activity" onDateChange={(date) => selectedDate = date}
            onYearChange={(year) => { selectedDate = null; void controller.loadRelationshipYear(year); }} />
        {:else}
          <EmptyState title="No items match this view" />
        {/if}
      </div>
    {/if}
    <div class="tabs">
      <SegmentedControl ariaLabel="Person tabs" value={details ? 'details' : filesOpen ? 'files' : 'messages'}
        options={[{ value: 'messages', label: 'Messages' }, { value: 'files', label: 'Files' }, { value: 'details', label: 'Details' }]}
        onchange={(value) => { details = value === 'details'; if (!details && filesOpen !== (value === 'files')) onFilesToggle(value === 'files'); }} />
    </div>
    {#if !details && (scope || (filesOpen && profileID !== null))}
      {#key `${target}:${controller.activityRevision}:${detail?.cache_revision}`}
        {#if filesOpen}
          {@render files()}
        {:else if scope}
          <PersonMessages {client} target={target!} predicate={context} identityScope={scope} {period} {onFiltersChange} {onOpenPerson} {onOpenMeeting} {onOpenSettings} {onMessagesTableChange} onOpenFiles={() => onFilesToggle(true)} onClearPeriod={() => selectedDate = null} />
        {/if}
      {/key}
    {/if}
  {/if}
  <div class="details" hidden={!details} data-scroll-pane>
    {#if profileID !== null && details}
      {#key profileID}<PersonDetails {onOpenMeeting} {sweepConfigured} {client} {directory} personID={profileID} onOpenPerson={onOpenDirectoryPerson} {onAnnounce} {onOpenSettings} />{/key}
    {/if}
    {#if detail && !controller.detailMissing}
      <PersonAddresses {detail} {onAnnounce} listedInDetails={savedProfile?.structuredProfile?.contact_points ?? []} onUnlinkParticipants={(a, b) => controller.unlinkParticipants(a, b).then(refreshSaved)} />
      {#if controller.headerState === 'no-mail'}
        <EmptyLine message={`No messages with ${savedProfile?.person?.display_name ?? person?.display_label ?? 'this person'} yet`} />
      {:else if 'domain' in detail}
        <p>{detail.activity_count.toLocaleString()} items{#if detail.activity_count > 0 && detail.first_at && detail.last_at} · {formatContactDate(detail.first_at)} – {formatContactDate(detail.last_at)}{/if} · {(detail as DomainSummary).person_count.toLocaleString()} people</p>
      {/if}
    {/if}
  </div>
  {#if mergeOpen}<MergePeopleDialog {client} {controller} personID={profileID} participantID={person?.id ?? null} {name} onReconcile={onReconcilePersonMerge} onLinked={linked} onOpenPerson={onOpenDirectoryPerson} onClose={() => mergeOpen = false} />{/if}
</section>

<style>
  .header-status { color: var(--text-muted); font-size: var(--type-meta-size); line-height: var(--type-meta-line); }
  .person-page { display: flex; min-width: 0; min-height: 0; width: 100%; height: 100%; flex-direction: column; overflow: auto; }
  /* On Details the header and tabs stay put and the Details pane scrolls under them. */
  .person-page.details-open { overflow: hidden; }
  .calendar { width: 100%; max-width: 960px; margin-inline: auto; padding: var(--pane-pad-y) var(--page-gutter); box-sizing: border-box; }
  /* Messages | Files | Details: a 28 segmented control 8 below the header, at the pane's 16 inset. */
  .tabs { flex: none; padding: var(--space-group) var(--space-pane); border-bottom: 1px solid var(--border-muted); }
  .files { display: flex; min-height: 0; flex: 1; padding: var(--pane-pad-y) var(--page-gutter) var(--page-gutter); }
  /* The Details pane reserves the 8px bar (data-scroll-pane pads its right side 8 less than its left), so rows
   * keep a 16 inset on both sides and never shift when the bar appears. 16 above, 24 between sections, and
   * 32 after the last one. It fills its column up to 880. */
  .details:not([hidden]) { display: flex; min-height: 0; flex: 1; flex-direction: column; gap: var(--space-section); padding-block: var(--space-pane) var(--space-end); }
  .details > :global(*) { width: 100%; max-width: 880px; box-sizing: border-box; }
  .details > :global(p) { margin: 0; color: var(--text-muted); font-size: var(--type-meta-size); line-height: var(--type-meta-line); }
  .summary-error { display: flex; align-items: center; gap: var(--space-group); padding: var(--space-group) var(--space-pane); }
</style>
