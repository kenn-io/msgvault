<script lang="ts">
  import { Button, Disclosure, IconButton, NumberInput, TextInput } from '@kenn-io/kit-ui';
  import X from '@lucide/svelte/icons/x';
  import Repeat from '@lucide/svelte/icons/repeat';
  import { onDestroy, untrack } from 'svelte';
  import DetailRow from '../people/DetailRow.svelte';
  import {
    createPersonAgendaItem,
    getKataIntegrationStatus,
    listPersonAgenda,
    unlinkPersonAgendaItem,
  } from '../../api/generated/api/api';
  import type { PersonAgendaItem as AgendaItem, PersonAgendaSchedule as AgendaSchedule } from '../../api/generated/models';
  import type { APIClient } from '../../api/client';
  import { orvalFetch } from '../../api/runtime';

  interface Props {
    client: APIClient;
    personID: number;
    refreshKey?: string;
    onSetUp?: () => void;
    onAnnounce?: (message: string) => void;
  }

  let { client, personID, refreshKey, onSetUp, onAnnounce = () => undefined }: Props = $props();
  let items = $state<AgendaItem[]>([]);
  let loading = $state(true);
  let mutating = $state(false);
  let error = $state('');
  let integrationState = $state('loading');
  let integrationMessage = $state('');
  let truncated = $state(false);
  let recurrenceState = $state('');
  let recurrenceMessage = $state('');
  let pendingUnlink = $state<AgendaItem | null>(null);
  let managedItem = $state<AgendaItem | null>(null);
  let managedSchedule = $state<AgendaSchedule | null>(null);
  let scheduleAction = $state('');
  let scheduleEditing = $state(false);
  let scheduleConflict = $state(false);
  let scheduleDraft = $state({ trigger_kind: 'cron', cron: '', interval_seconds: 60, timezone: 'UTC', occurrence_title: '', occurrence_body: '', recipient: '', catchup: 'latest', grace_seconds: 0 });
  let reviewedScheduleDraft = $state('');
  let unlinkTrigger: HTMLElement | null = null;
  let title = $state('');
  let listName = $state('agenda');
  let generation = 0;
  let mutationToken = 0;
  let controller: AbortController | null = null;
  let retryRequestID = '';
  let retryFingerprint = '';
  let selectedPersonID: number | undefined;
  let selectedClient: APIClient | undefined;
  let loadedRefreshKey: string | undefined;

  const mutationReady = $derived(integrationState === 'ready');
  const roots = $derived(items.filter(item => item.kind !== 'occurrence' || !items.some(parent => parent.kind === 'series' && parent.uid === item.parent_uid)));
  const stateMessage = $derived.by(() => {
    if (integrationState === 'loading') return '';
    if (integrationState !== 'ready') {
      return `Kata integration ${integrationState}: ${integrationMessage || 'No status detail was provided.'}`;
    }
    return '';
  });

  $effect(() => {
    const nextPersonID = personID;
    const nextClient = client;
    const nextRefreshKey = refreshKey;
    const pending = mutating;
    untrack(() => {
      const selectionChanged = selectedPersonID !== nextPersonID || selectedClient !== nextClient;
      if (selectionChanged) {
        selectedPersonID = nextPersonID;
        selectedClient = nextClient;
        retryRequestID = '';
        retryFingerprint = '';
        // Release the new person's controls while the prior person's write
        // settles. Its token can no longer change this selection's state.
        mutationToken++;
        mutating = false;
        pendingUnlink = null;
        managedItem = null;
        managedSchedule = null;
        scheduleAction = '';
        scheduleEditing = false;
        scheduleConflict = false;
        unlinkTrigger = null;
        recurrenceState = '';
        recurrenceMessage = '';
      }
      // Same-person revisions keep write ownership and retry protection.
      // Revisit a deferred revision when the pending write releases the form.
      if (selectionChanged || (nextRefreshKey !== loadedRefreshKey && !pending)) {
        items = [];
        error = '';
        integrationState = 'loading';
        integrationMessage = '';
        truncated = false;
        void load(nextPersonID);
      }
    });
  });

  onDestroy(() => controller?.abort());

  async function load(selectedPersonID = personID): Promise<void> {
    // A successful write's reload also covers revisions deferred during it.
    loadedRefreshKey = refreshKey;
    const current = ++generation;
    controller?.abort();
    controller = new AbortController();
    loading = true;
    error = '';
    try {
      const status = await getKataIntegrationStatus({ ...client, signal: controller.signal });
      if (current !== generation) return;
      integrationState = status.data?.state ?? 'unavailable';
      integrationMessage = status.data?.message ?? 'Task integration status is unavailable.';
      // The list endpoint 503s under the same conditions the mutations
      // check, so skip it and let the status message be the single
      // explanation instead of pairing it with a redundant
      // service-unavailable error.
      if (integrationState !== 'ready') {
        items = [];
        truncated = false;
        recurrenceState = '';
        recurrenceMessage = '';
        return;
      }
      const { data, error: responseError } = await listPersonAgenda(
        { id: selectedPersonID },
        { ...client, signal: controller.signal },
      );
      if (current !== generation) return;
      if (!data) throw new Error(messageFor(responseError, 'Task service is unavailable'));
      items = data.items ?? [];
      truncated = data.truncated;
      recurrenceState = data.recurrence?.state ?? '';
      recurrenceMessage = data.recurrence?.message ?? '';
    } catch (cause) {
      if (current !== generation || controller.signal.aborted) return;
      items = [];
      truncated = false;
      recurrenceState = '';
      recurrenceMessage = '';
      error = cause instanceof Error ? cause.message : 'Task service is unavailable';
    } finally {
      if (current === generation) loading = false;
    }
  }

  // Mirror the server's list canonicalization (trim, collapse internal
  // whitespace runs, lowercase, empty → 'agenda') exactly: the retry
  // fingerprint and the sent payload must both use the canonical form, or a
  // case/spacing change would rotate the idempotency key after an
  // unknown-outcome create and duplicate the task.
  function canonicalList(value: string): string {
    return value.trim().replace(/\s+/g, ' ').toLowerCase() || 'agenda';
  }

  async function createItem(): Promise<void> {
    const trimmedTitle = title.trim();
    const effectiveList = canonicalList(listName);
    if (!trimmedTitle || mutating || !mutationReady) return;
    // Bind the retry key to the exact submission (person, title, list). An
    // unchanged retry keeps the key so the server can deduplicate an unknown
    // outcome; any payload change must not replay the prior request, so it
    // mints a fresh key instead.
    const fingerprint = `${personID}\u0000${trimmedTitle}\u0000${effectiveList}`;
    if (!retryRequestID || retryFingerprint !== fingerprint) {
      retryRequestID = globalThis.crypto.randomUUID();
      retryFingerprint = fingerprint;
    }
    // Mutation requests carry no abort signal, so the selection can change
    // while one is in flight. Pin the submission to the current load
    // generation: when that generation moves on, drop every outcome — error,
    // title reset, retry-key reset, announcement, reload — so the prior
    // person's result is never written into the new person's view.
    const current = generation;
    const token = ++mutationToken;
    mutating = true;
    error = '';
    try {
      const { data, error: responseError } = await createPersonAgendaItem(
        { id: personID },
        { title: trimmedTitle, list: effectiveList },
        { ...client, headers: { 'Idempotency-Key': retryRequestID } },
      );
      if (current !== generation) return;
      if (!data) throw new Error(messageFor(responseError, 'Unable to add agenda item'));
      title = '';
      retryRequestID = '';
      retryFingerprint = '';
      onAnnounce('Agenda item added.');
      await load();
    } catch (cause) {
      if (current !== generation) return;
      error = cause instanceof Error ? cause.message : 'Unable to add agenda item';
    } finally {
      // Only the newest mutation owns the mutating flag: after a selection
      // change invalidated this token, the new person's state (possibly a
      // newer mutation's) must not be touched here.
      if (token === mutationToken) mutating = false;
    }
  }

  async function unlink(ref: string): Promise<void> {
    if (mutating || !mutationReady) return;
    // Same generation pin as createItem: mutation requests carry no abort
    // signal, so outcomes settled after a selection change must be dropped.
    const current = generation;
    const token = ++mutationToken;
    mutating = true;
    error = '';
    try {
      const { data, error: responseError } = await unlinkPersonAgendaItem(
        { id: personID, ref },
        client,
      );
      if (current !== generation) return;
      if (!data) throw new Error(messageFor(responseError, 'Unable to unlink agenda item'));
      onAnnounce('Agenda item unlinked.');
      await load();
    } catch (cause) {
      if (current !== generation) return;
      error = cause instanceof Error ? cause.message : 'Unable to unlink agenda item';
    } finally {
      // Same token ownership as createItem.
      if (token === mutationToken) mutating = false;
    }
  }

  function requestUnlink(item: AgendaItem): void {
    if (item.kind === 'series' || (recurrenceState && recurrenceState !== 'ready')) {
      unlinkTrigger = document.activeElement instanceof HTMLElement ? document.activeElement : null;
      pendingUnlink = item;
    } else void unlink(item.ref);
  }

  function focusUnlinkConfirmation(node: HTMLElement): void {
    node.querySelector<HTMLButtonElement>('button')?.focus();
  }

  function cancelUnlink(): void {
    pendingUnlink = null;
    unlinkTrigger?.focus();
    unlinkTrigger = null;
  }

  function manageSchedule(item: AgendaItem, schedule: AgendaSchedule): void {
    managedItem = item;
    managedSchedule = schedule;
    scheduleAction = '';
    scheduleEditing = false;
    scheduleConflict = false;
    scheduleDraft = {
      trigger_kind: schedule.trigger_kind, cron: schedule.cron ?? '', interval_seconds: schedule.interval_seconds,
      timezone: schedule.timezone, occurrence_title: schedule.occurrence_title,
      occurrence_body: schedule.occurrence_body ?? '', recipient: schedule.recipient,
      catchup: schedule.catchup, grace_seconds: schedule.grace_seconds,
    };
  }

  async function submitSchedule(): Promise<void> {
    if (!managedItem || !managedSchedule || !scheduleAction || mutating || scheduleConflict || (scheduleAction === 'edit' && reviewedScheduleDraft !== JSON.stringify(scheduleDraft))) return;
    const item = managedItem;
    const schedule = managedSchedule;
    const current = generation;
    const token = ++mutationToken;
    const action = scheduleAction;
    let responseReceived = false;
    mutating = true;
    error = '';
    try {
      const result = await orvalFetch<{ schedule: AgendaSchedule }>({
        url: `/api/v1/people/${personID}/agenda/${encodeURIComponent(item.uid)}/schedules/${encodeURIComponent(schedule.uid)}/actions`,
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        data: { action, expected_event_uid: schedule.definition_event_uid, ...(action === 'edit' ? { settings: scheduleDraft } : {}) },
      }, client);
      if (current !== generation) return;
      responseReceived = true;
      if (!result.data) {
        if (result.response.status === 409 || result.response.status === 503 || result.error?.error === 'unsupported_schedule') scheduleConflict = true;
        throw new Error(messageFor(result.error, 'Unable to update schedule. Refresh before retrying.'));
      }
      const confirmed = result.data.schedule;
      items = items.map(existing => existing.uid === item.uid
        ? { ...existing, schedules: existing.schedules?.map(value => value.uid === confirmed.uid ? confirmed : value) }
        : existing);
      managedSchedule = confirmed;
      scheduleAction = '';
      scheduleEditing = false;
      onAnnounce('Schedule updated.');
    } catch (cause) {
      if (current !== generation) return;
      if (!responseReceived) scheduleConflict = true;
      error = cause instanceof Error ? cause.message : 'Unable to update schedule';
    } finally {
      if (token === mutationToken) mutating = false;
    }
  }

  function scheduledTime(item: AgendaItem, parent: AgendaItem | undefined = undefined): string {
    const raw = item.occurrence?.scheduled_at;
    if (!raw || !Number.isFinite(Date.parse(raw))) return 'Scheduled time unavailable';
    const match = parent?.schedules?.find(schedule => schedule.uid === item.occurrence?.job_uid);
    const timezone = match?.timezone || 'UTC';
    const showTimezone = !match || !match.timezone || !parent?.schedules?.every(schedule => schedule.timezone === timezone);
    try {
      return `Scheduled for ${new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'short', timeZone: timezone }).format(new Date(raw))}${showTimezone ? ` · ${timezone}` : ''}`;
    } catch {
      return `Scheduled for ${raw}`;
    }
  }

  function scheduleState(state: string): string {
    const states: Record<string, string> = { active: 'Active', paused: 'Paused', parent_closed: 'Paused because parent is closed', deleted: 'Deleted schedule' };
    return states[state] ?? 'Unavailable';
  }

  function messageFor(value: unknown, fallback: string): string {
    return typeof value === 'object' && value !== null && 'message' in value && typeof value.message === 'string'
      ? value.message
      : fallback;
  }

  function label(value: string): string {
    return value.replace(/\b\w/g, (letter) => letter.toUpperCase());
  }
</script>

<section class="person-agenda" aria-labelledby={`person-${personID}-agenda-heading`}>
  <div class="heading"><h3 id={`person-${personID}-agenda-heading`}>Tasks</h3>{#if integrationState === 'disabled' && onSetUp}<Button size="sm" label="Set up" ariaLabel="Set up Tasks" onclick={onSetUp} />{/if}{#if loading}<span role="status">Loading…</span>{/if}</div>
  {#if error}<p class="notice" role="alert">{error}<Button size="sm" label="Retry tasks" onclick={() => void load()} /></p>{/if}
  {#if stateMessage && !(integrationState === 'disabled' && onSetUp)}<div class="heading"><span class="notice" role="status">{stateMessage}</span><Button size="sm" label="Set up" onclick={onSetUp} /></div>{/if}
  {#if recurrenceState && recurrenceState !== 'ready'}
    <div class="notice" role="status">
      <p>{recurrenceMessage || (recurrenceState === 'partial' ? 'Recurring schedules are incomplete.' : 'Recurring schedules unavailable.')}</p>
      <Button size="sm" label="Retry recurring schedules" onclick={() => void load()} disabled={loading || mutating} />
    </div>
  {/if}
  {#if !loading && !error && !stateMessage && (!recurrenceState || recurrenceState === 'ready') && items.length === 0}<p class="notice">No tasks yet</p>{/if}
  {#if truncated}<p class="notice" role="status">{recurrenceState === 'partial' ? 'Open Kata for the full view.' : 'The task view may be incomplete. Open Kata for the full view.'}</p>{/if}
  {#if pendingUnlink}
    <div class="unlink-series" role="group" aria-label={`Unlink ${pendingUnlink.title}`} use:focusUnlinkConfirmation>
      {#if pendingUnlink.kind === 'series'}
        <p>Unlink this series? It and its occurrences may stay visible through another linked series. Separately linked occurrences stay. Scheduling and inbox requests continue.</p>
      {:else}
        <p>Unlink this task? A series and its occurrences may stay visible through another linked series; an occurrence may stay through its linked series. Separately linked occurrences stay. Scheduling and inbox requests continue.</p>
      {/if}
      <Button size="sm" label="Unlink" disabled={mutating} onclick={() => { const ref = pendingUnlink?.ref; pendingUnlink = null; if (ref) void unlink(ref); }} />
      <Button size="sm" label={pendingUnlink.kind === 'series' ? 'Keep series linked' : 'Keep task linked'} onclick={cancelUnlink} />
    </div>
  {/if}
  {#if managedItem && managedSchedule}
    <div class="schedule-manager" role="group" aria-label={`Manage schedule for ${managedItem.title}`}>
      <p>{managedSchedule.summary} · {managedSchedule.timezone} · {scheduleState(managedSchedule.state)}</p>
      <p>{#if !scheduleEditing || scheduleDraft.recipient !== managedSchedule.recipient}Kata sends inbox requests to {managedSchedule.recipient}. {/if}Existing occurrences and inbox requests remain when a schedule is paused or deleted.</p>
      <p>{#if !scheduleEditing || scheduleDraft.catchup !== managedSchedule.catchup || scheduleDraft.grace_seconds !== managedSchedule.grace_seconds}{managedSchedule.catchup === 'skip' ? `Skip occurrences over ${managedSchedule.grace_seconds}s late.` : `Catch up ${managedSchedule.catchup === 'all' ? 'all missed occurrences' : 'only the latest occurrence'}.`} {/if}Resuming or restoring may deliver missed occurrences.</p>
      {#if scheduleEditing}
        <label>Repeat <select aria-label="Repeat" bind:value={scheduleDraft.trigger_kind}><option value="cron">Calendar schedule</option><option value="interval">Elapsed interval</option></select></label>
        {#if scheduleDraft.trigger_kind === 'cron'}<TextInput bind:value={scheduleDraft.cron} ariaLabel="Cron expression" />
        {:else}<label>Interval seconds <NumberInput ariaLabel="Interval seconds" min={60} value={scheduleDraft.interval_seconds} oninput={(value) => { scheduleDraft.interval_seconds = value ?? 0; }} /></label>{/if}
        <TextInput bind:value={scheduleDraft.timezone} ariaLabel="Timezone" />
        <TextInput bind:value={scheduleDraft.occurrence_title} ariaLabel="Occurrence title" />
        <label>Description <textarea bind:value={scheduleDraft.occurrence_body}></textarea></label>
        <label>Inbox recipient <TextInput bind:value={scheduleDraft.recipient} ariaLabel="Inbox recipient" /></label>
        <label>Missed occurrences <select aria-label="Missed occurrences" bind:value={scheduleDraft.catchup}><option value="all">Every missed occurrence</option><option value="latest">Only the latest</option><option value="skip">Skip old occurrences</option></select></label>
        {#if scheduleDraft.catchup === 'skip'}<label>Max age (s) <NumberInput ariaLabel="Max age (s)" min={1} value={scheduleDraft.grace_seconds} oninput={(value) => { scheduleDraft.grace_seconds = value ?? 0; }} /></label>{/if}
        <p>Daylight-saving changes can skip or repeat local times in calendar schedules.</p>
        <Button size="sm" label="Review schedule changes" disabled={mutating || scheduleConflict} onclick={() => { reviewedScheduleDraft = JSON.stringify(scheduleDraft); scheduleAction = 'edit'; }} />
      {:else}
        {#if !managedSchedule.deleted}
          <Button size="sm" label={managedSchedule.enabled ? 'Pause schedule' : 'Resume schedule'} disabled={mutating || scheduleConflict || (!managedSchedule.enabled && managedSchedule.parent_closed)} onclick={() => { scheduleAction = managedSchedule?.enabled ? 'pause' : 'resume'; }} />
          {#if managedSchedule.editable}<Button size="sm" label="Edit schedule" disabled={mutating || scheduleConflict} onclick={() => { scheduleEditing = true; scheduleAction = ''; }} />{/if}
          <Button size="sm" label="Delete schedule" disabled={mutating || scheduleConflict} onclick={() => { scheduleAction = 'delete'; }} />
        {:else}<Button size="sm" label="Restore schedule" disabled={mutating || scheduleConflict} onclick={() => { scheduleAction = 'restore'; }} />{/if}
      {/if}
      {#if scheduleAction && (scheduleAction !== 'edit' || reviewedScheduleDraft === JSON.stringify(scheduleDraft))}
        <p>{scheduleAction === 'edit' ? 'Save these schedule changes?' : `Confirm ${scheduleAction} of this schedule?`}</p>
        <Button size="sm" label={scheduleAction === 'edit' ? 'Save' : label(scheduleAction)} disabled={mutating || scheduleConflict} onclick={() => void submitSchedule()} />
      {/if}
      {#if scheduleConflict}<Button size="sm" label="Refresh schedule" disabled={loading || mutating} onclick={() => { managedItem = null; managedSchedule = null; scheduleConflict = false; void load(); }} />{/if}
      <Button size="sm" label="Close schedule" disabled={mutating} onclick={() => { managedItem = null; managedSchedule = null; }} />
    </div>
  {/if}
  {#each roots as item (item.uid)}
    <div class="agenda-family">
    <DetailRow>
      {#if item.web_url}<a href={item.web_url} target="_blank" rel="noreferrer">{item.title}</a>{:else}{item.title}{/if}
      {#if item.kind === 'series'}<span class="recurring-badge"><Repeat size={14} aria-hidden="true" />Recurring</span>{/if}
      {#if item.list !== 'agenda'}<small>{item.list}</small>{/if}
      {#if item.state !== 'open'}<small>{label(item.state)}</small>{/if}
      {#if item.status !== item.state}<small>{label(item.status)}</small>{/if}
      {#snippet actions()}{#if item.kind !== 'occurrence' && item.membership !== 'inherited'}<IconButton size="sm" ariaLabel={`Unlink ${item.title}`} disabled={mutating || !mutationReady} onclick={() => requestUnlink(item)}><X size={14} /></IconButton>{/if}{/snippet}
    </DetailRow>
    {#if item.kind === 'series'}
      {@const children = items.filter(child => child.kind === 'occurrence' && child.parent_uid === item.uid)}
      <div class="schedules">
        {#each item.schedules ?? [] as schedule (schedule.uid)}
          <p>{schedule.summary} · {schedule.timezone || 'UTC'} <small>{scheduleState(schedule.state)}</small>{#if item.membership !== 'inherited' && mutationReady}<Button size="sm" label="Manage" ariaLabel={`Manage schedule ${schedule.summary}`} disabled={mutating} onclick={() => manageSchedule(item, schedule)} />{/if}</p>
          {#if schedule.summary === 'Custom schedule' && schedule.cron}<Disclosure variant="plain" title="Schedule expression"><code>{schedule.cron}</code></Disclosure>{/if}
        {/each}
        {#if !item.occurrences_complete}<p class="notice">Occurrence count incomplete</p>
        {:else if !item.open_occurrences}<p class="notice">No open occurrences</p>
        {:else if item.open_occurrences > children.length}<p class="notice">{item.open_occurrences} open occurrence{item.open_occurrences === 1 ? '' : 's'}</p>{/if}
        {#if item.web_url}<a class="notice" href={item.web_url} target="_blank" rel="noreferrer">View completed occurrences in Kata</a>{/if}
      </div>
      {#if children.length}
        <ul class="occurrences" aria-label={`Occurrences of ${item.title}`}>
          {#each children as child (child.uid)}
            <li><DetailRow>
              {#if child.web_url}<a href={child.web_url} target="_blank" rel="noreferrer">{child.title}</a>{:else}{child.title}{/if}
              <small>{label(child.status)}</small>
              {#if child.list !== item.list}<small>{child.list}</small>{/if}
            </DetailRow><p class="notice">{scheduledTime(child, item)}</p></li>
          {/each}
        </ul>
      {/if}
    {:else if item.kind === 'occurrence'}<p class="notice">{scheduledTime(item)}</p>{/if}
    </div>
  {/each}
  {#if mutationReady}
    <form onsubmit={(event) => { event.preventDefault(); void createItem(); }}>
      <TextInput bind:value={title} ariaLabel="New agenda item" placeholder="Add a task" block disabled={mutating} />
      <TextInput bind:value={listName} ariaLabel="List" placeholder="agenda" disabled={mutating} />
      <Button type="submit" label="Add item" disabled={mutating || !title.trim()} />
    </form>
  {/if}

</section>

<style>
  .person-agenda { display: grid; gap: var(--space-2); min-width: 0; }
  .heading { min-height: var(--control-h); }
  .heading, form { display: flex; align-items: center; justify-content: space-between; gap: var(--control-gap); }
  h3, p { margin: 0; }
  h3 { font-size: var(--font-size-md); }
  form { flex-wrap: wrap; justify-content: flex-start; }
  form :global(.kit-text-input) { min-width: 0; flex: 1; }
  small { margin-left: var(--space-2); color: var(--text-muted); }
  a { color: var(--link-ink); }
  .notice { color: var(--text-muted); font-size: var(--font-size-sm); }
  .agenda-family { min-width: 0; }
  .recurring-badge { display: inline-flex; align-items: center; gap: var(--space-1); margin-left: var(--space-2); color: var(--text-muted); font-size: var(--font-size-sm); }
  .schedules { display: grid; gap: var(--space-1); }
  .schedules p { font-size: var(--font-size-sm); }
  .schedules code { overflow-wrap: anywhere; }
  .occurrences { display: grid; gap: var(--space-2); margin: var(--space-2) 0 0 var(--space-3); padding-left: var(--space-3); }
  .unlink-series { display: flex; flex-wrap: wrap; gap: var(--space-2); }
  .unlink-series p { flex-basis: 100%; font-size: var(--font-size-sm); }
</style>
