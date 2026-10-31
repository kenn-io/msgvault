<script lang="ts">
  import { Button, EmptyState, SegmentedControl } from '@kenn-io/kit-ui';
  import { untrack } from 'svelte';
  import type { APIClient } from '../../api/client';
  import { listPersonAttributes } from '../../api/generated/api/api';
  import type { MeetingRef } from '../../api/generated/models';
  import type { RelationshipsController } from '../../relationships/controller.svelte';
  import type { DomainSummary, PersonSummary, ExplorePredicate, FileSearchSort, FileMIMEFamily, PersonFileDirection } from '../../explore/models';
  import RelationshipHeader from '../relationships/RelationshipHeader.svelte';
  import RelationshipCalendar from '../relationships/RelationshipCalendar.svelte';
  import { localDayBoundsUTC } from '../relationships/timeline-support';
  import FilesWorkspace from '../files/FilesWorkspace.svelte';
  import PersonHeader from './PersonHeader.svelte';
  import PersonMessages from './PersonMessages.svelte';
  import DomainCalendar from './DomainCalendar.svelte';

  let { client, controller, target, filesOpen, predicate, onFilesToggle, onBack,
    onOpenDirectory, onOpenDirectoryPerson, onAnnounce, onOpenPerson, onOpenMeeting, onOpenSettings,
    onOpenFileItem, onOpenFileConversation, personFilePresentation = 'files', personFileDirections = ['from_person'],
    onPersonFilePresentationChange, onPersonFileDirectionsChange }:
    { client: APIClient; controller: RelationshipsController; target: string | null; filesOpen: boolean; predicate: ExplorePredicate;
      onFilesToggle: (value: boolean) => void; onBack?: () => void; onOpenDirectory?: (id: number) => void;
      onOpenDirectoryPerson?: (id: number) => void; onAnnounce?: (message: string) => void; onOpenPerson?: (id: number) => void;
      onOpenMeeting?: (meeting: MeetingRef) => void; onOpenSettings?: () => void; onOpenFileItem?: (key: string) => void;
      onOpenFileConversation?: (key: string, messageID: number, conversationID: number) => void;
      personFilePresentation?: 'media' | 'files'; personFileDirections?: PersonFileDirection[];
      onPersonFilePresentationChange?: (value: 'media' | 'files') => void; onPersonFileDirectionsChange?: (value: PersonFileDirection[]) => void } = $props();
  let details = $state(false);
  let selectedDate = $state<string | null>(null);
  let sort = $state<FileSearchSort>({ field: 'occurred_at', direction: 'desc' });
  let filenameQuery = $state('');
  let mimeFamilies = $state<FileMIMEFamily[]>([]);
  const domain = $derived(target?.startsWith('domain:') ? target.slice(7) : undefined);
  const detail = $derived(controller.target === target ? controller.detail : null);
  const person = $derived(detail && !domain ? detail as PersonSummary : null);
  const scope = $derived(domain ? { kind: 'domain' as const, domain } : controller.target === target && controller.canonicalID !== null ? { kind: 'person' as const, id: controller.canonicalID } : undefined);
  const name = $derived(domain ?? person?.display_label ?? 'Person');
  const email = $derived(person?.identifiers?.find((identity) => identity.type === 'email')?.value ?? '');
  const since = $derived(person?.first_at ? new Intl.DateTimeFormat('en-US', { month: 'short', year: 'numeric' }).format(new Date(person.first_at)) : undefined);
  const period = $derived.by(() => {
    if (!selectedDate) return null;
    if (domain) { const after = `${selectedDate}T00:00:00.000Z`; return { after, before: new Date(Date.parse(after) + 86400000).toISOString() }; }
    const bounds = localDayBoundsUTC(`${selectedDate}T00:00:00`);
    return { after: bounds.start, before: bounds.end };
  });
  $effect(() => { void target; untrack(() => { selectedDate = null; details = false; filenameQuery = ''; mimeFamilies = []; }); });
  $effect(() => { if (filesOpen) selectedDate = null; });
</script>

<section class="person-page" aria-label="Person page">
  {#if controller.timelineLoading && !detail}
    <p role="status">Loading…</p>
  {:else if controller.detailMissing}
    <EmptyState title="Person or domain not found" />
    {#if onBack}<Button label="Back to People" onclick={onBack} />{/if}
  {:else if !detail}
    <EmptyState title={controller.timelineError ?? 'Select a person or domain'} />
    {#if controller.timelineError && target}<Button label="Retry" onclick={() => target && controller.openTarget(target, predicate)} />{/if}
  {:else}
    <PersonHeader {name} {email} domain={Boolean(domain)} {onBack} onOpenDetails={() => details = true} />
    {#if !filesOpen && !details}
      <div class="calendar">
        {#if domain}
          <DomainCalendar {client} {domain} firstAt={(detail as DomainSummary).first_at} lastAt={(detail as DomainSummary).last_at} {selectedDate} onDateChange={(date) => selectedDate = date} />
        {:else}
          <RelationshipCalendar calendar={controller.relationshipCalendar} loading={controller.relationshipCalendarLoading} error={controller.relationshipCalendarError}
            year={controller.relationshipCalendarYear} firstYear={controller.relationshipCalendarFirstYear} currentYear={controller.relationshipCalendarCurrentYear}
            {selectedDate} {since} title="Activity" showTemperature={false} onDateChange={(date) => selectedDate = date}
            onYearChange={(year) => { selectedDate = null; void controller.loadRelationshipYear(year); }} />
        {/if}
      </div>
    {/if}
    <div class="tabs">
      <SegmentedControl ariaLabel="Person tabs" value={details ? 'details' : filesOpen ? 'files' : 'messages'}
        options={[{ value: 'messages', label: 'Messages' }, { value: 'files', label: 'Files' }, { value: 'details', label: 'Details' }]}
        onchange={(value) => { details = value === 'details'; if (!details && filesOpen !== (value === 'files')) onFilesToggle(value === 'files'); }} />
    </div>
    {#if details}
      <div class="details">
        <RelationshipHeader showTabs={false} {detail} loading={false} missing={false} {filesOpen} {onFilesToggle} {client} {onOpenDirectory} {onOpenDirectoryPerson} {onAnnounce}
          loadAttributes={async (id) => (await listPersonAttributes({ id }, { history: false }, client)).data?.attributes ?? []}
          capturePersonMergeContext={() => controller.personMergeContextSnapshot()}
          onReconcilePersonMerge={(context) => controller.reconcilePersonMerge(context)}
          onLinkParticipants={(a, b) => controller.linkParticipants(a, b)} onUnlinkParticipants={(a, b) => controller.unlinkParticipants(a, b)} />
      </div>
    {:else if scope}
      {#key target}
        {#if filesOpen}
          <div class="files"><FilesWorkspace {client} embedded showHeader={false} {predicate} identityScope={scope} {sort} {filenameQuery} {mimeFamilies}
            personPresentation={personFilePresentation} personDirections={personFileDirections} onSortChange={(value) => sort = value}
            onFilenameQueryChange={(value) => filenameQuery = value} onMIMEFamiliesChange={(value) => mimeFamilies = value}
            onPersonPresentationChange={onPersonFilePresentationChange} onPersonDirectionsChange={onPersonFileDirectionsChange}
            onOpenItem={onOpenFileItem} onOpenConversation={onOpenFileConversation} /></div>
        {:else}
          <PersonMessages {client} target={target!} {predicate} identityScope={scope} {period} {onOpenPerson} {onOpenMeeting} {onOpenSettings} onOpenFiles={() => onFilesToggle(true)} onClearPeriod={() => selectedDate = null} />
        {/if}
      {/key}
    {/if}
  {/if}
</section>

<style>
  .person-page { display: flex; min-width: 0; min-height: 0; width: 100%; height: 100%; flex-direction: column; overflow: auto; }
  .calendar { width: 100%; max-width: 960px; margin-inline: auto; padding: var(--pane-pad-y) var(--page-gutter); box-sizing: border-box; }
  .tabs { padding: var(--space-3) var(--page-gutter); border-bottom: 1px solid var(--border-muted); }
  .files { display: flex; min-height: 0; flex: 1; padding: var(--pane-pad-y) var(--page-gutter) var(--page-gutter); }
  .details { padding: var(--pane-pad-y) var(--page-gutter); }
</style>
