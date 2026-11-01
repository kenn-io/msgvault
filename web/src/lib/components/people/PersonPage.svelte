<script lang="ts">
  import { Button, EmptyState, SegmentedControl } from '@kenn-io/kit-ui';
  import type { APIClient } from '../../api/client';
  import type { DirectoryController } from '../../directory/controller.svelte';
  import PersonDetails from './PersonDetails.svelte';
  import PersonRecordActions from '../directory/PersonRecordActions.svelte';
  import type { MeetingRef } from '../../api/generated/models';
  import { contextPredicate, type RelationshipsController, type RelationshipsMergeContext } from '../../relationships/controller.svelte';
  import type { DomainSummary, PersonSummary, ExplorePredicate, FileSearchSort, FileMIMEFamily, PersonFileDirection, ExploreFilter } from '../../explore/models';
  import RelationshipHeader from '../relationships/RelationshipHeader.svelte';
  import RelationshipCalendar from '../relationships/RelationshipCalendar.svelte';
  import { localDayBoundsUTC } from '../relationships/timeline-support';
  import { RELATIONSHIP_READER_HISTORY_KEY, relationshipReaderHistory } from '../../relationships/reader-history';
  import FilesWorkspace from '../files/FilesWorkspace.svelte';
  import PersonHeader from './PersonHeader.svelte';
  import PersonMessages from './PersonMessages.svelte';
  import DomainCalendar from './DomainCalendar.svelte';

  let { client, directory, onRecordDeleted, onSplitCommitted, onOpenCardDAVConflict, onOpenCardDAVSettings, controller, target, filesOpen, predicate, onFilesToggle, onBack, durablePersonID = null,
    onConfigureVisualSearch, onOpenDirectory, onOpenDirectoryPerson, onAnnounce, onOpenPerson, onOpenMeeting, onOpenSettings, sweepConfigured,
    onOpenFileItem, onOpenFileConversation, onReconcilePersonMerge, personFilePresentation = 'files', personFileDirections = ['from_person'],
    onPersonFilePresentationChange, onPersonFileDirectionsChange, onFiltersChange,
    details = $bindable(false), selectedDate = $bindable<string | null>(null), domainYear = $bindable<number | null>(null),
    sort = $bindable<FileSearchSort>({ field: 'occurred_at', direction: 'desc' }), filenameQuery = $bindable(''), mimeFamilies = $bindable<FileMIMEFamily[]>([]) }:
    { client: APIClient; directory: DirectoryController; onRecordDeleted?: (personID: number) => void; onSplitCommitted?: (context: import('../../directory/person-merge-history-controller.svelte').PersonSplitCommittedContext) => void | Promise<void>; onOpenCardDAVConflict?: (id: number) => void; onOpenCardDAVSettings?: () => void; controller: RelationshipsController; target: string | null; filesOpen: boolean; predicate: ExplorePredicate; durablePersonID?: number | null;
      onReconcilePersonMerge: (context: RelationshipsMergeContext) => Promise<void>;
      onFilesToggle: (value: boolean) => void; onBack?: () => void; onOpenDirectory?: (id: number) => void;
      onOpenDirectoryPerson?: (id: number) => void; onAnnounce?: (message: string) => void; onOpenPerson?: (id: number) => void;
      sweepConfigured?: boolean; onConfigureVisualSearch?: () => void; onOpenMeeting?: (meeting: MeetingRef) => void; onOpenSettings?: (category?: string) => void; onOpenFileItem?: (key: string) => void;
      onOpenFileConversation?: (key: string, messageID: number, conversationID: number) => void;
      personFilePresentation?: 'media' | 'files'; personFileDirections?: PersonFileDirection[];
      details?: boolean; selectedDate?: string | null; domainYear?: number | null; sort?: FileSearchSort; filenameQuery?: string; mimeFamilies?: FileMIMEFamily[];
      onFiltersChange: (filters: ExploreFilter[]) => void;
      onPersonFilePresentationChange?: (value: 'media' | 'files') => void; onPersonFileDirectionsChange?: (value: PersonFileDirection[]) => void } = $props();
  const context = $derived(contextPredicate(predicate));
  const domain = $derived(target?.startsWith('domain:') ? target.slice(7) : undefined);
  const detail = $derived(controller.target === target ? controller.detail : null);
  const person = $derived(detail && !domain ? detail as PersonSummary : null);
  const scope = $derived(domain ? { kind: 'domain' as const, domain } : controller.target === target && controller.canonicalID !== null ? { kind: 'person' as const, id: controller.canonicalID } : undefined);
  const profileID = $derived(durablePersonID ?? person?.profile?.id ?? null);
  const savedProfile = $derived(directory.selectedPersonID === profileID ? directory.profile : null);
  const name = $derived(domain ?? savedProfile?.person?.display_name ?? person?.display_label ?? 'Person');
  let saving = $state(false);
  async function savePerson(): Promise<void> {
    if (!person || saving) return;
    saving = true;
    try { const result = await directory.promote(person.id); if (result.ok) await controller.loadList(predicate); } finally { saving = false; }
  }
  const email = $derived(person?.identifiers?.find((identity) => identity.type === 'email')?.value ?? '');
  const since = $derived(controller.firstContactAt ? new Intl.DateTimeFormat('en-US', { month: 'short', year: 'numeric' }).format(new Date(controller.firstContactAt)) : undefined);
  const period = $derived.by(() => {
    if (!selectedDate) return null;
    const bounds = localDayBoundsUTC(`${selectedDate}T00:00:00`);
    return { after: bounds.start, before: bounds.end };
  });
  // Files pushes its own entry first, so this drops the cleared day only from that entry's reader.
  function clearDate(): void {
    selectedDate = null;
    const reader = relationshipReaderHistory(window.history.state, 'relationships', target);
    if (reader) window.history.replaceState({ ...window.history.state, [RELATIONSHIP_READER_HISTORY_KEY]: { ...reader, bounds: undefined } }, '');
  }
  $effect(() => { if (filesOpen) clearDate(); });
  async function refreshSaved<T extends { ok: boolean }>(outcome: T): Promise<T> {
    if (outcome.ok && savedProfile) await directory.refreshSelectedPerson();
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

<section class="person-page" role="group" aria-label="Person page">
  {#if durablePersonID !== null || (detail && !controller.detailMissing)}
    <PersonHeader {name} {email} domain={Boolean(domain)} {onBack} onOpenDetails={() => details = true}>
      {#snippet actions()}
        {#if profileID !== null && savedProfile}{#key savedProfile}<PersonRecordActions {client} controller={savedProfile} personID={profileID} onOpenPerson={onOpenDirectoryPerson} {onAnnounce} onDeleted={onRecordDeleted} {onSplitCommitted} {onOpenCardDAVConflict} {onOpenCardDAVSettings} />{/key}
        {:else if person && profileID === null}<Button label={saving ? 'Saving…' : 'Save'} disabled={saving} onclick={() => void savePerson()} />{/if}
      {/snippet}
    </PersonHeader>
  {/if}
  {#if controller.timelineLoading && !detail && durablePersonID === null}
    <p role="status">Loading…</p>{#if onBack}<Button label="Back to People" onclick={onBack} />{/if}
  {:else if durablePersonID !== null && (!detail || controller.detailMissing)}
    {#if controller.timelineLoading}<p role="status">Loading…</p>{/if}
    {#if controller.timelineError}<div class="summary-error" role="alert"><span>{controller.timelineError}</span><Button label="Retry" onclick={() => target && controller.openTarget(target, predicate, false)} /></div>{/if}
    <div class="tabs"><SegmentedControl ariaLabel="Person tabs" value={filesOpen && !details ? 'files' : 'details'} options={[{ value: 'files', label: 'Files' }, { value: 'details', label: 'Details' }]} onchange={(value) => { details = value === 'details'; onFilesToggle(value === 'files'); }} /></div>
    {#if filesOpen && !details}{@render files()}{/if}
  {:else if controller.detailMissing}
    <EmptyState title="Person or domain not found" />
    {#if onBack}<Button label="Back to People" onclick={onBack} />{/if}
  {:else if !detail}
    <EmptyState title={controller.timelineError ?? 'Select a person or domain'} />
    {#if onBack}<Button label="Back to People" onclick={onBack} />{/if}
    {#if controller.timelineError && target}<Button label="Retry" onclick={() => target && controller.openTarget(target, predicate, false)} />{/if}
  {:else}
    {#if directory.promotionError}<p role="alert">{directory.promotionError}<Button label="Retry Save" onclick={() => void savePerson()} /></p>{/if}
    {#if controller.timelineError}
      <div class="summary-error" role="alert"><span>{controller.timelineError}</span><Button label="Retry" onclick={() => target && controller.openTarget(target, predicate, false)} /></div>
    {/if}
    {#if !filesOpen && !details}
      <div class="calendar">
        {#if domain}
          <DomainCalendar bind:year={domainYear} {client} {domain} firstAt={controller.firstContactAt} lastAt={controller.lastContactAt} {selectedDate} onDateChange={(date) => selectedDate = date} />
        {:else}
          <RelationshipCalendar calendar={controller.relationshipCalendar} loading={controller.relationshipCalendarLoading} error={controller.relationshipCalendarError}
            year={controller.relationshipCalendarYear} firstYear={controller.relationshipCalendarFirstYear} currentYear={controller.relationshipCalendarCurrentYear}
            {selectedDate} {since} title="Activity" onDateChange={(date) => selectedDate = date}
            onYearChange={(year) => { selectedDate = null; void controller.loadRelationshipYear(year); }} />
        {/if}
      </div>
    {/if}
    <div class="tabs">
      <SegmentedControl ariaLabel="Person tabs" value={details ? 'details' : filesOpen ? 'files' : 'messages'}
        options={[{ value: 'messages', label: 'Messages' }, { value: 'files', label: 'Files' }, { value: 'details', label: 'Details' }]}
        onchange={(value) => { details = value === 'details'; if (!details && filesOpen !== (value === 'files')) onFilesToggle(value === 'files'); }} />
    </div>
    {#if !details && scope}
      {#key `${target}:${controller.activityRevision}`}
        {#if filesOpen}
          {@render files()}
        {:else}
          <PersonMessages {client} target={target!} predicate={context} identityScope={scope} {period} {onFiltersChange} {onOpenPerson} {onOpenMeeting} {onOpenSettings} onOpenFiles={() => onFilesToggle(true)} onClearPeriod={() => selectedDate = null} />
        {/if}
      {/key}
    {/if}
  {/if}
  <div class="details" hidden={!(details || (durablePersonID !== null && (!detail || controller.detailMissing) && !filesOpen))}>
    {#if profileID !== null && (details || (durablePersonID !== null && (!detail || controller.detailMissing) && !filesOpen))}
      {#key profileID}<PersonDetails {onOpenMeeting} {sweepConfigured} {client} {directory} personID={profileID} onOpenPerson={onOpenDirectoryPerson} {onAnnounce} {onOpenSettings} />{/key}
    {/if}
    {#if detail && !controller.detailMissing}
      <RelationshipHeader {onOpenDirectoryPerson} showIdentity={false} showTabs={false} title="Identities" {detail} loading={false} missing={false} {filesOpen} {onFilesToggle} {client} {onAnnounce}
        capturePersonMergeContext={() => controller.personMergeContextSnapshot()} {onReconcilePersonMerge}
        onLinkParticipants={(a, b) => controller.linkParticipants(a, b).then(refreshSaved)} onUnlinkParticipants={(a, b) => controller.unlinkParticipants(a, b).then(refreshSaved)} />
    {/if}
  </div>
</section>

<style>
  .person-page { display: flex; min-width: 0; min-height: 0; width: 100%; height: 100%; flex-direction: column; overflow: auto; }
  .calendar { width: 100%; max-width: 960px; margin-inline: auto; padding: var(--pane-pad-y) var(--page-gutter); box-sizing: border-box; }
  .tabs { padding: var(--space-3) var(--page-gutter); border-bottom: 1px solid var(--border-muted); }
  .files { display: flex; min-height: 0; flex: 1; padding: var(--pane-pad-y) var(--page-gutter) var(--page-gutter); }
  .details:not([hidden]) { display: flex; flex-direction: column; gap: var(--space-7); padding: var(--pane-pad-y) var(--page-gutter) var(--space-8); }
  .details > :global(*) { max-width: 720px; }
  .summary-error { display: flex; align-items: center; gap: var(--space-3); padding: var(--pane-pad-y) var(--page-gutter); }
</style>
