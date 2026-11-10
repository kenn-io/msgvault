<script lang="ts">
  import { listDirectoryPeople as generatedListDirectoryPeople } from '../../api/generated/api/api';
  import {
    debounce,
    type TypeaheadOption,
  } from '@kenn-io/kit-ui';
  import { onDestroy, onMount, untrack } from 'svelte';
  import type { APIClient } from '../../api/client';
  import { formatPartialDate as partialDate } from '../../directory/dates';
  import type { DirectoryEntityController } from '../../directory/entity-controller.svelte';
  import type {
    CreatePersonRelationshipRequest,
    DirectoryPerson,
    PatchPersonRelationshipRequest,
    PersonRelationship,
    PersonRelationshipView,
    Prepared,
  } from '../../directory/models';
  import FieldPicker from '../common/FieldPicker.svelte';
  import Dialog from '../common/Dialog.svelte';
  import DateField from '../common/DateField.svelte';
  import DateRange from '../common/DateRange.svelte';
  import InlineNotice from '../common/InlineNotice.svelte';
  import IdentityAvatar from '../common/IdentityAvatar.svelte';
  import { sourcePhrase } from './history-format';
  import { sentenceCase } from '../../explore/labels';
  import ConflictNotice from '../common/ConflictNotice.svelte';
  import { connectionFacts } from '../../directory/conflict';
  import { WriteAttempt } from '../../directory/write-attempt.svelte';
  import { relationshipPhrase, relationshipTypeOptions, type RelationshipChoice } from '../../directory/relationship-type-options';
  const SEARCH_LIMIT = 20;
  const SEARCH_DEBOUNCE_MS = 250;
  interface Props {
    client: APIClient;
    controller: DirectoryEntityController;
    personID: number;
    personName?: string;
    relationship?: PersonRelationshipView;
    onDone?: () => void;
    onClose?: () => void;
  }
  let {
    client,
    controller,
    personID,
    personName = 'This person',
    relationship = undefined,
    onDone = () => undefined,
    onClose = () => undefined,
  }: Props = $props();
  const initialView = untrack(() => relationship);
  let initialRelationship = $state(initialView?.relationship);
  let opened = $state<Prepared<PersonRelationship>>();
  let counterpartID = $state<number | null>(initialView?.counterpart_person_id ?? null);
  let people = $state<DirectoryPerson[]>([]);
  let searching = $state(false);
  let searchError = $state('');
  let lookupQuery: string | undefined;
  let searchText = $state('');
  let typeQuery = $state('');
  let sentence: HTMLDivElement | undefined = $state();
  let choice = $state('');
  const typeOptions = $derived(relationshipTypeOptions(controller.relationshipTypes));
  const selectedType = $derived(typeOptions.flatMap(group => group.children ?? []).find(option => option.name === choice) as RelationshipChoice | undefined);
  const relationshipTypeSlug = $derived(initialRelationship?.type_slug ?? selectedType?.slug ?? '');
  const direction = $derived(selectedType?.direction ?? 'outgoing');
  // "Since 2022 · from Google contacts": how long they've known each other, and where it came from when someone else recorded it.
  const connectionSince = $derived(initialRelationship ? [
    initialRelationship.start_date?.year ? `Since ${initialRelationship.start_date.year}` : '',
    sourcePhrase(initialRelationship.source),
  ].filter(Boolean).join(' · ').replace(/^./, first => first.toUpperCase()) : '');
  const editLabel = $derived(initialView?.direction === 'incoming' ? initialRelationship?.reverse_label ?? '' : initialRelationship?.forward_label ?? '');
  // A seeded type reads as a sentence; a custom or renamed label can't be put in one, so it stands apart, as in the row.
  const editPhrase = $derived(initialRelationship ? relationshipPhrase(initialRelationship.type_slug, initialView?.direction === 'incoming' ? 'incoming' : 'outgoing', editLabel) : '');
  let startDate = $state(partialDate(initialView?.relationship.start_date));
  let endDate = $state(partialDate(initialView?.relationship.end_date));
  let datesInvalid = $state(false);
  let endDateError = $state<string | null>(null);
  let notes = $state(initialView?.relationship.notes ?? '');
  const attempt = new WriteAttempt<Prepared<PersonRelationship>>();
  let committed = $state(false);
  const loading = $derived(!!initialView && !opened && !attempt.details);
  const locked = $derived(attempt.busy || committed || controller.createBlocked.relationships);

  let searchAbort: AbortController | undefined;
  let searchGeneration = 0;
  const peopleOptions = $derived(
    people.map(
      (person): TypeaheadOption => ({
        name: String(person.id),
        label: person.display_name?.trim() || 'Unnamed person',
        meta: person.organizations.join(', '),
      }),
    ),
  );
  const debouncedSearch = debounce((value: string) => void searchPeople(value), SEARCH_DEBOUNCE_MS);
  onMount(() => {
    if (initialView) void prepare();
  });
  onDestroy(() => {
    debouncedSearch.cancel();
    searchAbort?.abort();
  });
  async function prepare(): Promise<void> {
    if (!initialView) return;
    opened = await attempt.prepare(() => controller.prepareRelationshipMutation(initialView.relationship.id));
    if (!opened) return;
    initialRelationship = opened.data;
    startDate = partialDate(opened.data.start_date);
    endDate = partialDate(opened.data.end_date);
    notes = opened.data.notes ?? '';
  }
  async function searchPeople(value: string): Promise<void> {
    const query = value.trim();
    searchAbort?.abort();
    const abort = new AbortController();
    searchAbort = abort;
    const generation = ++searchGeneration;
    searching = true;
    searchError = '';
    try {
      const response = await generatedListDirectoryPeople(
        { ...(query ? { q: query } : {}), sort: 'last_contact_desc', limit: SEARCH_LIMIT },
        {
          ...client,
          signal: abort.signal,
        },
      );
      if (abort.signal.aborted || generation !== searchGeneration) return;
      if (response.data) {
        people = (response.data.people ?? []).filter((person) => person.id !== personID);
      } else {
        searchError = failureMessage(response.error, response.response.status);
      }
    } catch (cause: unknown) {
      if (!abort.signal.aborted && generation === searchGeneration) searchError = failureMessage(cause, 0);
    } finally {
      if (generation === searchGeneration) searching = false;
    }
  }
  function handleQuery(value: string): void {
    const query = value.trim();
    searchText = query;
    if (query === lookupQuery && !searchError) return;
    debouncedSearch.cancel();
    searchAbort?.abort();
    searchGeneration++;
    lookupQuery = query;
    people = [];
    searchError = '';
    searching = true;
    if (query) debouncedSearch(query);
    else void searchPeople('');
  }
  function selectCounterpart(value: string): void {
    const id = Number(value);
    counterpartID = id;
  }
  async function submit(): Promise<void> {
    if (
      attempt.busy || committed || loading || (!!initialView && !opened) ||
      datesInvalid || !!endDateError ||
      !relationshipTypeSlug ||
      (!initialRelationship && (counterpartID === null || counterpartID === personID))
    )
      return;
    const patch = initialRelationship ? patchBody() : undefined;
    const create = initialRelationship ? undefined : createBody(counterpartID!);
    const ok = await attempt.run(current => initialRelationship
      ? controller.updateRelationship(initialRelationship.id, patch!, current ?? opened)
      : controller.createRelationship(create!));
    if (ok) {
      if (controller.errors.relationships) committed = true;
      else onDone();
    }
  }

  function createBody(otherID: number): CreatePersonRelationshipRequest {
    return {
      source_person_id: direction === 'outgoing' ? personID : otherID,
      target_person_id: direction === 'outgoing' ? otherID : personID,
      relationship_type_slug: relationshipTypeSlug,
      ...(startDate.trim() ? { start_date: startDate.trim() } : {}),
      ...(endDate.trim() ? { end_date: endDate.trim() } : {}),
      ...(notes.trim() ? { notes: notes.trim() } : {}),
    };
  }
  // The form as shown; an emptied End date isn't sent, since a connection reopens only as a new one.
  function patchBody(): PatchPersonRelationshipRequest {
    return { ...(endDate.trim() ? { end_date: endDate.trim() } : {}), notes: notes.trim() || null };
  }
  async function refreshAfterUncertainOutcome(): Promise<void> {
    await controller.refreshRelationships();
    if (!controller.createBlocked.relationships && !controller.errors.relationships) onDone();
  }
  function requestClose(): void {
    if (!attempt.busy) { if (attempt.hadConflict) void controller.refreshRelationships(); attempt.reset(); onClose(); }
  }
  function failureMessage(value: unknown, status: number): string {
    if (typeof value === 'object' && value !== null && 'message' in value && typeof value.message === 'string')
      return value.message;
    if (value instanceof Error && value.message) return value.message;
    return status ? `The server answered ${status}.` : 'No response from the server.';
  }
</script>

<Dialog title={initialRelationship ? 'Edit connection' : 'Add connection'} onclose={requestClose}
  primaryLabel={attempt.conflict ? 'Keep yours' : initialRelationship ? 'Save' : 'Add connection'} onprimary={() => void submit()}
  cancelLabel={attempt.conflict ? 'Use saved' : 'Cancel'}
  busy={attempt.busy} primaryDisabled={committed || loading || (!!initialView && !opened) || !relationshipTypeSlug || datesInvalid || !!endDateError || (!initialRelationship && (counterpartID === null || counterpartID === personID)) || controller.createBlocked.relationships}>
  {#if loading}<p role="status">Loading connection…</p>{:else if !initialView || opened}
  <form onsubmit={(event) => { event.preventDefault(); void submit(); }}>
    <div class="editor" aria-busy={attempt.busy}>
      <section>
        {#if initialRelationship}
          <div class="connection-row">
            <p><strong>{personName}</strong>{editPhrase === editLabel ? ` · ${sentenceCase(editLabel)} · ` : ` ${editPhrase} `}<strong>{initialView?.counterpart_display_name?.trim() || 'Unnamed person'}</strong></p>
            <span class="muted">{connectionSince}</span>
          </div>
        {:else}
          <div class="sentence" bind:this={sentence}>
            <span class="sentence-start">
            <strong>{personName}</strong>
            <span class="type-picker">
              <FieldPicker options={typeOptions} value={choice} fallbackLabel="Type" placeholder="Connection type" title="Connection type"
                highlightFirstOption panelAnchor={sentence} onquery={(query) => { typeQuery = query; }}
                emptyLabel={controller.errors.relationshipTypes ? '' : typeQuery ? `No type matches “${typeQuery}”` : 'No connection types yet'}
                onretry={() => void controller.refreshRelationshipTypes()}
                loading={controller.relationshipTypesLoading} error={controller.errors.relationshipTypes ? "Couldn't load connection types." : ''} errorDetails={controller.errors.relationshipTypes} disabled={locked}
                onselect={(value) => { choice = value; }} />
            </span>
            </span>
            <span class="people-picker">
              <FieldPicker remote options={peopleOptions} value={counterpartID === null ? '' : String(counterpartID)}
                panelAnchor={sentence} disabled={locked}
                fallbackLabel="Person" placeholder="Person" title="Person"
                emptyLabel={searchError ? '' : searchText ? `No one matches “${searchText}”` : 'No other saved people'}
                loading={searching} loadingLabel="Searching…" onquery={handleQuery} error={searchError ? "Couldn't search people." : ''} errorDetails={searchError} onretry={() => void searchPeople(searchText)}
                onselect={selectCounterpart}>
                {#snippet icon(option)}<IdentityAvatar label={option.label} seed={option.name} size={24} />{/snippet}
              </FieldPicker>
            </span>
          </div>
        {/if}
      </section>
      <section>
        {#if initialRelationship}
          <DateField bind:value={endDate} start={startDate} bind:error={endDateError} disabled={locked} />
          {#if (attempt.conflict?.data.end_date ?? initialRelationship.end_date) && !endDate.trim()}<p class="muted">To reopen it, add a new connection.</p>{/if}
        {:else}<DateRange bind:start={startDate} bind:end={endDate} showCurrent={false} bind:invalid={datesInvalid} disabled={locked} />{/if}
        <label class="notes">Notes<textarea aria-label="Notes" bind:value={notes} disabled={locked}></textarea></label>
      </section>
    </div>
  <button type="submit" hidden>Save</button>
</form>{/if}
  {#snippet notice()}
    {#if committed}
      <InlineNotice message="The relationship was saved, but the list could not be refreshed." details={controller.errors.relationships} actionLabel="Check again" onaction={() => void refreshAfterUncertainOutcome()} />
    {:else if initialView && !opened && !loading}
      <InlineNotice message="Couldn't load this connection." details={attempt.details} actionLabel="Retry" onaction={() => void prepare()} />
    {:else if attempt.conflict}
      <ConflictNotice noun="connection" saved={attempt.conflict.data} record={connectionFacts(attempt.conflict.data)} message={attempt.details} />
    {:else if attempt.details || controller.createBlocked.relationships}
      <InlineNotice message={controller.createBlocked.relationships ? "Couldn't confirm the connection was saved." : "Couldn't save this connection."} details={attempt.details || undefined}
        actionLabel={controller.createBlocked.relationships ? 'Check again' : 'Retry'} onaction={() => { if (controller.createBlocked.relationships) void refreshAfterUncertainOutcome(); else void submit(); }} />
    {/if}
  {/snippet}
</Dialog>

<style>
  /* Two groups 24 apart: who is connected, then when and why. Inside a group, fields are 16 apart. */
  .editor { display: grid; gap: var(--space-section); min-width: 0; }
  section { display: grid; gap: var(--space-pane); min-width: 0; }
  .notes { display: grid; gap: var(--space-tight); color: var(--text-secondary); font-size: var(--type-meta-size); font-weight: var(--font-weight-semibold); line-height: var(--type-meta-line); }
  textarea { resize: vertical; border: 1px solid var(--border-default); border-radius: var(--radius-sm); background: var(--bg-canvas); color: var(--text-primary); font: inherit; font-weight: normal; }
  p { margin: 0; }
  /* The sentence sits on the date pair's two columns, 12 apart, so its edges are theirs: the name (Body) and Type
   * share the first, 8 apart, ending where Start date does, and Person fills the second, starting where End date does.
   * On a phone the name takes its own line and each picker is full width, 16 apart, like stacked fields. */
  .sentence { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); align-items: center; gap: var(--space-pane) var(--space-box); min-width: 0; font-size: var(--type-body-size); line-height: var(--type-body-line); }
  .sentence-start { display: grid; grid-template-columns: fit-content(50%) minmax(0, 1fr); align-items: center; gap: var(--space-group); min-width: 0; }
  @media (max-width: 640px) {
    .sentence, .sentence-start { grid-template-columns: minmax(0, 1fr); gap: var(--space-pane); }
  }
  .sentence strong { min-width: 0; overflow-wrap: anywhere; }
  /* Edit connection names the two people (Body), then since when (Meta), as one two-line row. */
  .connection-row { display: grid; min-width: 0; }
  .connection-row p { min-width: 0; overflow-wrap: anywhere; font-size: var(--type-body-size); line-height: var(--type-body-line); }
  .type-picker {
    min-width: 0;
    --typeahead-panel-min-width: 0px;
  }
  .people-picker {
    min-width: 0;
    --typeahead-icon-width: 24px;
    --typeahead-panel-min-width: 0px;
  }
  .sentence :global(.kit-typeahead) {
    width: 100%;
    --typeahead-min-width: 0px;
    --typeahead-max-width: 100%;
  }
  .type-picker :global(.kit-typeahead__option--group) {
    background: transparent;
  }
  .muted { color: var(--text-muted); font-size: var(--type-meta-size); line-height: var(--type-meta-line); }
</style>
