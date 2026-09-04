<script lang="ts">
  import {
    appShortcuts,
    Button,
    debounce,
    Modal,
    SelectDropdown,
    TextInput,
    Typeahead,
    type TypeaheadOption
  } from '@kenn-io/kit-ui';
  import { onDestroy, onMount, untrack } from 'svelte';

  import type { APIClient } from '../../api/client';
  import type { DirectoryEntityController } from '../../directory/entity-controller.svelte';
  import type {
    CreatePersonRelationshipRequest,
    DirectoryPerson,
    PatchPersonRelationshipRequest,
    PersonRelationship,
    PersonRelationshipView
  } from '../../directory/models';

  const SEARCH_LIMIT = 20;
  const SEARCH_DEBOUNCE_MS = 250;

  interface Props {
    client: APIClient;
    controller: DirectoryEntityController;
    personID: number;
    relationship?: PersonRelationshipView;
    onDone?: () => void;
    onClose?: () => void;
  }

  let {
    client,
    controller,
    personID,
    relationship = undefined,
    onDone = () => undefined,
    onClose = () => undefined
  }: Props = $props();
  const initialView = untrack(() => relationship);
  const initialRelationship = initialView?.relationship;
  let counterpartID = $state<number | null>(initialView?.counterpart_person_id ?? null);
  let people = $state<DirectoryPerson[]>([]);
  let searching = $state(false);
  let searchError = $state('');
  let preserveSelectionOnClose = false;
  let direction = $state<'outgoing' | 'incoming'>('outgoing');
  let relationshipTypeSlug = $state(initialRelationship?.type_slug ?? '');
  let startDate = $state(partialDate(initialRelationship?.start_date));
  let endDate = $state(partialDate(initialRelationship?.end_date));
  let notes = $state(initialRelationship?.notes ?? '');
  let submitting = $state(false);
  let committed = $state(false);
  let message = $state('');
  let conflictCurrent = $state<PersonRelationship>();
  let searchAbort: AbortController | undefined;
  let searchGeneration = 0;
  let releaseScope: (() => void) | undefined;

  const peopleOptions = $derived(people.map((person): TypeaheadOption => ({
    name: String(person.id),
    label: person.display_name?.trim() || `Person ${person.id}`,
    meta: person.organizations.length > 0 ? person.organizations.join(', ') : 'Durable person'
  })));
  const typeOptions = $derived(controller.relationshipTypes.map((type) => ({
    value: type.slug,
    label: type.is_symmetric ? type.forward_label : `${type.forward_label} / ${type.reverse_label}`
  })));
  const directionOptions = [
    { value: 'outgoing', label: 'Selected person → counterpart' },
    { value: 'incoming', label: 'Counterpart → selected person' }
  ];
  const debouncedSearch = debounce((value: string) => void searchPeople(value), SEARCH_DEBOUNCE_MS);

  onMount(() => {
    releaseScope = appShortcuts.pushScope('directory-relationship-editor');
  });

  onDestroy(() => {
    debouncedSearch.cancel();
    searchAbort?.abort();
    releaseScope?.();
  });

  async function searchPeople(value: string): Promise<void> {
    const query = value.trim();
    searchAbort?.abort();
    if (!query) {
      people = [];
      searching = false;
      searchError = '';
      return;
    }
    const abort = new AbortController();
    searchAbort = abort;
    const generation = ++searchGeneration;
    searching = true;
    searchError = '';
    try {
      const response = await client.GET('/api/v1/people/directory', {
        params: { query: { q: query, limit: SEARCH_LIMIT } },
        signal: abort.signal
      });
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
    if (!value.trim() && preserveSelectionOnClose) return;
    counterpartID = null;
    preserveSelectionOnClose = false;
    debouncedSearch(value);
  }

  function selectCounterpart(value: string): void {
    const id = Number(value);
    counterpartID = id === personID ? null : id;
    preserveSelectionOnClose = counterpartID !== null;
    message = counterpartID === null ? 'A person cannot have a relationship with itself.' : '';
  }

  async function submit(): Promise<void> {
    if (submitting || committed || !relationshipTypeSlug || (!initialRelationship && (counterpartID === null || counterpartID === personID))) return;
    const patch = initialRelationship ? patchBody() : undefined;
    if (patch && Object.keys(patch).length === 0) {
      onDone();
      return;
    }
    submitting = true;
    message = '';
    conflictCurrent = undefined;
    try {
      const result = initialRelationship
        ? await controller.updateRelationship(initialRelationship.id, patch!)
        : await controller.createRelationship(createBody(counterpartID!));
      if (result.ok) {
        if (controller.errors.relationships) {
          committed = true;
          message = `The relationship was saved, but the list could not be refreshed. ${controller.errors.relationships}`;
        } else {
          onDone();
        }
        return;
      }
      if (result.kind === 'conflict') {
        conflictCurrent = result.current;
        message = `This relationship changed elsewhere. ${result.message}`;
      } else if (result.kind === 'unknown' || result.kind === 'blocked') {
        message = `The create outcome is unknown. ${result.message}`;
      } else {
        message = result.message;
      }
    } finally {
      submitting = false;
    }
  }

  function createBody(otherID: number): CreatePersonRelationshipRequest {
    return {
      source_person_id: direction === 'outgoing' ? personID : otherID,
      target_person_id: direction === 'outgoing' ? otherID : personID,
      relationship_type_slug: relationshipTypeSlug,
      ...(startDate.trim() ? { start_date: startDate.trim() } : {}),
      ...(endDate.trim() ? { end_date: endDate.trim() } : {}),
      ...(notes.trim() ? { notes: notes.trim() } : {})
    };
  }

  function patchBody(): PatchPersonRelationshipRequest {
    const body: PatchPersonRelationshipRequest = {};
    const nextEndDate = endDate.trim();
    const openingEndDate = partialDate(initialRelationship?.end_date);
    const nextNotes = notes.trim();
    const openingNotes = initialRelationship?.notes?.trim() ?? '';
    if (nextEndDate !== openingEndDate) body.end_date = nextEndDate;
    if (nextNotes !== openingNotes) body.notes = nextNotes || null;
    return body;
  }

  async function refreshAfterUncertainOutcome(): Promise<void> {
    await controller.refreshRelationships();
    if (!controller.createBlocked.relationships && !controller.errors.relationships) onDone();
  }

  function requestClose(): void {
    if (!submitting) onClose();
  }

  function partialDate(value: { year?: number; month?: number; day?: number } | undefined): string {
    if (!value) return '';
    return [
      value.year?.toString().padStart(4, '0'),
      value.month?.toString().padStart(2, '0'),
      value.day?.toString().padStart(2, '0')
    ].filter(Boolean).join('-');
  }

  function failureMessage(value: unknown, status: number): string {
    if (typeof value === 'object' && value !== null && 'message' in value && typeof value.message === 'string') return value.message;
    if (value instanceof Error && value.message) return value.message;
    return status ? `Request failed (${status}).` : 'Request failed.';
  }
</script>

<Modal
  title={initialRelationship ? 'Edit relationship' : 'Add relationship'}
  ariaLabel={initialRelationship ? 'Edit relationship' : 'Add relationship'}
  closeLabel="Close relationship editor"
  onclose={requestClose}
>
  <form class="editor" aria-busy={submitting} onsubmit={(event) => { event.preventDefault(); void submit(); }}>
    {#if initialRelationship}
      <p><strong>{initialView?.counterpart_display_name?.trim() || initialView?.counterpart_vcard_uid || `Person ${initialView?.counterpart_person_id}`}</strong> · {initialView?.counterpart_label}</p>
      <p class="muted">Type: {initialRelationship.type_slug}. End date and notes are editable; edge direction and type stay fixed.</p>
    {:else}
      <label>Counterpart
        <Typeahead
          options={peopleOptions}
          value={counterpartID === null ? '' : String(counterpartID)}
          fallbackLabel="Choose a durable person"
          placeholder="Relationship counterpart"
          title="Relationship counterpart"
          emptyLabel="No matching durable people"
          loading={searching}
          loadingLabel="Searching…"
          remote
          onquery={handleQuery}
          error={searchError}
          onselect={selectCounterpart}
        />
      </label>
      <label>Direction<SelectDropdown title="Relationship direction" value={direction} options={directionOptions} onchange={(value) => { direction = value as typeof direction; }} disabled={submitting} /></label>
      <label>Type<SelectDropdown title="Relationship type" value={relationshipTypeSlug} options={typeOptions} onchange={(value) => { relationshipTypeSlug = value; }} disabled={submitting} /></label>
      <label>Start date<TextInput ariaLabel="Relationship start date" bind:value={startDate} placeholder="YYYY, YYYY-MM, or YYYY-MM-DD" block disabled={submitting} /></label>
    {/if}
    <label>End date<TextInput ariaLabel="Relationship end date" bind:value={endDate} placeholder="YYYY, YYYY-MM, or YYYY-MM-DD" block disabled={submitting} /></label>
    <label>Notes<TextInput ariaLabel="Relationship notes" bind:value={notes} block disabled={submitting} /></label>
    {#if message}
      <div role="alert">
        <p>{message}</p>
        {#if conflictCurrent}
          <p>Current record: {partialDate(conflictCurrent.end_date) || 'no end date'} · {conflictCurrent.notes || 'no notes'}.</p>
        {/if}
      </div>
    {/if}
    {#if controller.createBlocked.relationships || committed}
      <Button label="Refresh relationships" disabled={submitting} onclick={() => void refreshAfterUncertainOutcome()} />
    {/if}
    <div class="actions">
      <Button label="Cancel" disabled={submitting} onclick={requestClose} />
      <Button
        type="submit"
        tone="info"
        surface="solid"
        label={initialRelationship ? 'Save relationship' : 'Create relationship'}
        disabled={submitting || committed || !relationshipTypeSlug || (!initialRelationship && (counterpartID === null || counterpartID === personID)) || controller.createBlocked.relationships}
      />
    </div>
  </form>
</Modal>

<style>
  .editor { display: grid; gap: var(--space-3); min-width: min(30rem, 80vw); }
  label { display: grid; gap: var(--space-1); color: var(--text-muted); font-size: var(--font-size-xs); }
  p { margin: 0; }
  .muted { color: var(--text-muted); font-size: var(--font-size-sm); }
  [role="alert"] { display: grid; gap: var(--space-1); color: var(--text-danger); }
  .actions { display: flex; justify-content: flex-end; gap: var(--space-2); flex-wrap: wrap; }
</style>
