<script lang="ts">
  import XIcon from '@lucide/svelte/icons/x';
  import { Button, DetailDrawer, IconButton, Modal, SearchInput, SelectDropdown, TextInput } from '@kenn-io/kit-ui';
  import { onDestroy, onMount, tick, untrack } from 'svelte';

  import type { MeetingRef } from '../../api/generated/models';
  import { deletePerson as generatedDeletePerson } from '../../api/generated/api/api';
  import type { APIClient } from '../../api/client';
  import type { DirectoryURLState } from '../../directory/models';
  import { DirectoryController } from '../../directory/controller.svelte';
  import { PRIMARY_CHANNELS, channelLabel, contactStateLabel, formatContactDate, formatDay } from '../../directory/labels';
  import { ExploreSelectionState } from '../../explore/state.svelte';
  import { bufferedCallback } from '../../util/buffered-callback';
  import DirectoryList from './DirectoryList.svelte';
  import PageHeader from '../shell/PageHeader.svelte';
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
    onOpenMeeting?: (meeting: MeetingRef) => void;
    onOpenRelationship?: (participantID: number) => void;
    onReviewFacts?: (personID: number) => void;
  }

  let {
    client,
    controller,
    state: urlState,
    promotionParticipantID = undefined,
    onOpenCardDAVConflict = () => undefined,
    onOpenCardDAVSettings = () => undefined,
    onAnnounce = () => undefined,
    onOpenMeeting = undefined,
    onOpenRelationship = undefined,
    onReviewFacts = undefined
  }: Props = $props();
  let root = $state<HTMLElement>();
  let narrow = $state(false);
  let mediaQuery: MediaQueryList | undefined;
  const listSelection = new ExploreSelectionState();
  let bulkDeletePending = $state(false);
  let bulkMessage = $state<string | null>(null);
  let bulkError = $state(false);
  let bulkFailures = $state<Array<{ name: string; message: string }>>([]);
  let confirmBulkDelete = $state(false);
  const selectedPeople = $derived(
    controller.rows.filter((person) => listSelection.isSelected(String(person.id)))
  );
  const directoryFilterFingerprint = $derived(JSON.stringify([
    urlState.directoryQuery,
    urlState.directoryContactState,
    urlState.directoryCategory,
    urlState.directoryOrganization,
    urlState.directoryPrimaryChannel,
    urlState.directoryLastContactAfter,
    urlState.directoryLastContactBefore,
    urlState.directorySort
  ]));
  let followedFilterFingerprint = untrack(() => directoryFilterFingerprint);

  const TEXT_FILTER_DEBOUNCE_MS = 250;
  type TextFilterKey = 'directoryQuery' | 'directoryCategory' | 'directoryOrganization';
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
      directoryOrganization: controller.organization
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
    { value: 'active', label: contactStateLabel('active') },
    { value: 'inactive', label: contactStateLabel('inactive') }
  ];
  const primaryChannelOptions = [
    { value: '', label: 'All channels' },
    ...PRIMARY_CHANNELS.map((value) => ({ value, label: channelLabel(value) }))
  ];
  const sortOptions = [
    { value: 'name', label: 'Name' },
    { value: 'last_contact_desc', label: 'Most recently contacted' },
    { value: 'last_contact_asc', label: 'Least recently contacted' }
  ].map((option) => ({ ...option, triggerLabel: `Sort: ${option.label}` }));

  let filtersOpen = $state(false);

  interface FilterChip {
    key: keyof DirectoryURLState;
    text: string;
  }

  // One chip per active filter, in the order the panel lists the controls.
  const filterChips = $derived.by((): FilterChip[] => {
    const chips: FilterChip[] = [];
    if (controller.contactState) chips.push({ key: 'directoryContactState', text: `Contact state: ${contactStateLabel(controller.contactState)}` });
    if (controller.category) chips.push({ key: 'directoryCategory', text: `Category: ${controller.category}` });
    if (controller.organization) chips.push({ key: 'directoryOrganization', text: `Organization: ${controller.organization}` });
    if (controller.primaryChannel) chips.push({ key: 'directoryPrimaryChannel', text: `Primary channel: ${channelLabel(controller.primaryChannel)}` });
    if (controller.lastContactAfter) chips.push({ key: 'directoryLastContactAfter', text: `Last contacted after ${formatDay(controller.lastContactAfter)}` });
    if (controller.lastContactBefore) chips.push({ key: 'directoryLastContactBefore', text: `Last contacted before ${formatDay(controller.lastContactBefore)}` });
    return chips;
  });

  const countLabel = $derived(
    controller.loading && controller.rows.length === 0
      ? ''
      : `${controller.rows.length.toLocaleString()}${controller.cursor !== null ? '+' : ''} ${controller.rows.length === 1 && controller.cursor === null ? 'person' : 'people'}`
  );

  $effect(() => {
    // Read every URL field outside untrack so AppShell history restoration
    // rehydrates this controller, but keep controller internals untracked:
    // selecting a row must never be mistaken for a URL change and cleared.
    const snapshot: DirectoryURLState = { ...urlState };
    untrack(() => controller.applyURLState(snapshot));
  });

  $effect(() => {
    const next = directoryFilterFingerprint;
    if (next === followedFilterFingerprint) return;
    followedFilterFingerprint = next;
    listSelection.clear();
    bulkMessage = null;
    bulkError = false;
    bulkFailures = [];
    confirmBulkDelete = false;
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

  function beginBulkDelete(): void {
    if (selectedPeople.length === 0 || bulkDeletePending) return;
    confirmBulkDelete = true;
  }

  async function deleteSelectedPeople(): Promise<void> {
    if (selectedPeople.length === 0 || bulkDeletePending) return;
    const targets = [...selectedPeople];
    bulkDeletePending = true;
    bulkMessage = null;
    bulkError = false;
    bulkFailures = [];
    const deletedIDs: number[] = [];
    // The server explains how to unblock each person (unpublish from
    // CardDAV, split a merge), so keep its message per person.
    const failures: Array<{ name: string; message: string }> = [];
    for (const person of targets) {
      const name = person.display_name ?? `Person ${person.id}`;
      try {
        const response = await generatedDeletePerson(
          { id: person.id },
          {
            ...client,
            headers: { 'If-Match': `"person-${person.id}-r${person.revision}"` }
          }
        );
        if (response.response.status === 204) deletedIDs.push(person.id);
        else failures.push({ name, message: deleteFailureMessage(response.error, response.response.status) });
      } catch (cause: unknown) {
        failures.push({ name, message: deleteFailureMessage(cause, 0) });
      }
    }
    for (const personID of deletedIDs) listSelection.explicitKeys.delete(String(personID));
    await controller.removeDeletedPeople(deletedIDs);
    confirmBulkDelete = false;
    bulkDeletePending = false;
    if (failures.length === 0) {
      listSelection.clear();
      bulkMessage = `${deletedIDs.length.toLocaleString()} ${deletedIDs.length === 1 ? 'person' : 'people'} deleted.`;
      return;
    }
    bulkError = true;
    bulkFailures = failures;
    bulkMessage = `${deletedIDs.length.toLocaleString()} deleted; ${failures.length.toLocaleString()} could not be deleted:`;
  }

  function deleteFailureMessage(error: unknown, status: number): string {
    if (typeof error === 'object' && error !== null && 'message' in error && typeof error.message === 'string' && error.message) {
      return error.message;
    }
    if (error instanceof Error && error.message) return error.message;
    return status ? `Request failed (${status})` : 'Request failed';
  }
</script>

<main class="directory-workspace" bind:this={root} aria-label="Directory">
  <PageHeader title="Directory" description="People you've saved, with profiles and contact details.">
    {#snippet actions()}
      {#if promotionParticipantID !== undefined}
        <Button label="Promote to person" tone="info" surface="solid" onclick={() => void promote()} />
      {/if}
    {/snippet}
  </PageHeader>
  <div class="directory-controls">
    <div class="directory-toolbar">
      <SearchInput value={textFilters.directoryQuery} ariaLabel="Search directory" placeholder="Search people, email, or organization…" block oninput={(value) => editTextFilter('directoryQuery', value)} />
      <Button size="sm" surface={filtersOpen || filterChips.length > 0 ? 'soft' : 'outline'} label="Filters" ariaLabel="Filters"
        ariaExpanded={filtersOpen} onclick={() => { filtersOpen = !filtersOpen; }} />
      <SelectDropdown title="Directory order" value={controller.sort} options={sortOptions}
        onchange={(value) => selectFilter({ directorySort: value as DirectoryURLState['directorySort'] })} />
      <span class="directory-count" aria-live="polite">{countLabel}</span>
    </div>
    {#if filtersOpen}
      <div class="filter-panel" role="group" aria-label="Directory filters">
        <SelectDropdown title="Contact state" value={controller.contactState} options={contactStateOptions}
          onchange={(value) => selectFilter({ directoryContactState: value })} />
        <TextInput value={textFilters.directoryCategory} ariaLabel="Category filter" placeholder="Category"
          oninput={(value) => editTextFilter('directoryCategory', value)} />
        <TextInput value={textFilters.directoryOrganization} ariaLabel="Organization filter" placeholder="Organization"
          oninput={(value) => editTextFilter('directoryOrganization', value)} />
        <SelectDropdown title="Primary channel" value={controller.primaryChannel} options={primaryChannelOptions}
          onchange={(value) => selectFilter({ directoryPrimaryChannel: value })} />
        <span class="date-field">
          <label>
            Last contacted after
            <!-- kit-ui-check-ignore: a one-sided boundary needs a single optional date; kit DateRangePicker only commits complete ranges (PR 3 spec, Directory list). -->
            <input type="date" aria-label="Last contacted after" value={controller.lastContactAfter}
              onchange={(event) => selectFilter({ directoryLastContactAfter: event.currentTarget.value })} />
          </label>
          {#if controller.lastContactAfter}
            <IconButton size="sm" ariaLabel="Clear last contacted after"
              onclick={() => selectFilter({ directoryLastContactAfter: '' })}><XIcon size="12" aria-hidden="true" /></IconButton>
          {/if}
        </span>
        <span class="date-field">
          <label>
            Last contacted before
            <!-- kit-ui-check-ignore: a one-sided boundary needs a single optional date; kit DateRangePicker only commits complete ranges (PR 3 spec, Directory list). -->
            <input type="date" aria-label="Last contacted before" value={controller.lastContactBefore}
              onchange={(event) => selectFilter({ directoryLastContactBefore: event.currentTarget.value })} />
          </label>
          {#if controller.lastContactBefore}
            <IconButton size="sm" ariaLabel="Clear last contacted before"
              onclick={() => selectFilter({ directoryLastContactBefore: '' })}><XIcon size="12" aria-hidden="true" /></IconButton>
          {/if}
        </span>
      </div>
    {/if}
    {#if filterChips.length > 0}
      <div class="filter-chips">
        {#each filterChips as chip (chip.key)}
          <span class="chip">
            {chip.text}
            <IconButton size="sm" ariaLabel={`Remove ${chip.text} filter`}
              onclick={() => selectFilter({ [chip.key]: '' })}><XIcon size="12" aria-hidden="true" /></IconButton>
          </span>
        {/each}
      </div>
    {/if}
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
      selection={listSelection}
      bulkPending={bulkDeletePending}
      {bulkMessage}
      {bulkError}
      {bulkFailures}
      onSelect={(personID) => void controller.selectPerson(personID)}
      onBulkDelete={beginBulkDelete}
      onLoadMore={() => void controller.loadNextPage()}
      onReload={() => void controller.reloadFirstPage()}
    />
    {#if controller.selectedPersonID !== null && !narrow}
      <aside class="detail-pane" aria-label="Person detail">
        {#if controller.detailLoading}<p role="status">Loading person detail…</p>{:else if controller.detail}<PersonDetail {client} bundle={controller.detail} personID={controller.selectedPersonID} profileController={controller.profile} entityController={controller.entity} onOpenPerson={(personID) => void controller.selectPerson(personID)} onSplitCommitted={(context) => controller.reconcilePersonSplit(context)} {onOpenCardDAVConflict} {onOpenCardDAVSettings} {onAnnounce} {onOpenMeeting} {onOpenRelationship} {onReviewFacts} />{/if}
      </aside>
    {/if}
  </div>
  {#if controller.selectedPersonID !== null && narrow}
    <DetailDrawer title="Person detail" ariaLabel="Person detail" onclose={() => void closeDetail()}>
      {#if controller.detailLoading}<p role="status">Loading person detail…</p>{:else if controller.detail}<PersonDetail {client} bundle={controller.detail} personID={controller.selectedPersonID} profileController={controller.profile} entityController={controller.entity} onOpenPerson={(personID) => void controller.selectPerson(personID)} onSplitCommitted={(context) => controller.reconcilePersonSplit(context)} {onOpenCardDAVConflict} {onOpenCardDAVSettings} {onAnnounce} {onOpenMeeting} {onOpenRelationship} {onReviewFacts} />{/if}
    </DetailDrawer>
  {/if}
</main>

{#if confirmBulkDelete}
  <Modal
    title={`Delete ${selectedPeople.length.toLocaleString()} ${selectedPeople.length === 1 ? 'person' : 'people'}?`}
    tone="danger"
    onclose={() => { if (!bulkDeletePending) confirmBulkDelete = false; }}
  >
    <p>
      This permanently deletes the selected durable {selectedPeople.length === 1 ? 'person profile' : 'person profiles'},
      removes participant bindings, and retires {selectedPeople.length === 1 ? 'its' : 'their'} vCard UID.
    </p>
    {#snippet footer()}
      <Button surface="soft" label="Cancel" disabled={bulkDeletePending} onclick={() => { confirmBulkDelete = false; }} />
      <Button
        tone="danger"
        surface="solid"
        label={bulkDeletePending ? 'Deleting…' : 'Confirm delete'}
        disabled={bulkDeletePending}
        onclick={() => void deleteSelectedPeople()}
      />
    {/snippet}
  </Modal>
{/if}

<style>
  .directory-workspace { padding: var(--space-5) var(--page-gutter) var(--space-4); display: grid; gap: var(--space-4); flex: 1; min-height: 0; grid-template-rows: auto auto auto minmax(0, 1fr); overflow: hidden; }
  .directory-controls { display: grid; gap: var(--space-2); min-width: 0; }
  .directory-toolbar, .filter-chips { display: flex; gap: var(--space-3); align-items: center; flex-wrap: wrap; min-width: 0; }
  .directory-toolbar :global(.kit-search-input) { min-width: min(100%, 300px); flex: 1; }
  .directory-count { margin-left: auto; color: var(--text-muted); font-size: var(--font-size-xs); white-space: nowrap; }
  .filter-panel { display: flex; gap: var(--space-3); align-items: center; flex-wrap: wrap; padding: var(--space-3); border: 1px solid var(--border-default); border-radius: var(--radius-md); background: var(--bg-surface); }
  .date-field { display: inline-flex; gap: var(--space-2); align-items: center; color: var(--text-secondary); font-size: var(--font-size-xs); }
  .date-field label { display: inline-flex; gap: var(--space-2); align-items: center; }
  .chip { display: inline-flex; max-width: 100%; align-items: center; gap: var(--space-1); padding: 0 0 0 var(--space-2); border: 1px solid color-mix(in srgb, var(--accent-amber) 35%, var(--border-muted)); border-radius: var(--radius-sm); background: color-mix(in srgb, var(--accent-amber) 8%, var(--bg-surface)); color: var(--text-secondary); font-size: var(--font-size-xs); overflow-wrap: anywhere; }
  .directory-content { display: grid; grid-row: 4; min-height: 0; overflow: hidden; }
  .directory-content > :global(*) { min-height: 0; overflow: auto; }
  .directory-content.has-detail { grid-template-columns: minmax(260px, 0.8fr) minmax(360px, 1.2fr); gap: var(--space-4); }
  .detail-pane { border-left: 1px solid var(--border-default); min-width: 0; min-height: 0; overflow: auto; }
  .promotion-error { padding: var(--space-3); background: var(--bg-inset); color: var(--text-secondary); }
</style>
