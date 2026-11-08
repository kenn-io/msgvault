<script lang="ts">
  import { Button, NumberInput, TextInput } from '@kenn-io/kit-ui';
  import { onDestroy, onMount } from 'svelte';
  import type { APIClient } from '../../api/client';
  import type { PersonAgendaItem, PersonAgendaSchedule } from '../../api/generated/models';
  import { orvalFetch } from '../../api/runtime';

  interface Draft {
    key: string;
    attempted: boolean;
    project?: string;
    target?: string;
    title: string;
    body: string;
    list: string;
    owner: string;
    settings: { trigger_kind: string; cron: string; interval_seconds: number; timezone: string; occurrence_title: string; occurrence_body: string; recipient: string; catchup: string; grace_seconds: number };
  }
  interface Props {
    client: APIClient;
    personID: number;
    personUIDs: string[];
    project: string;
    target: string;
    onCreated: (parent: PersonAgendaItem, schedule: PersonAgendaSchedule) => void;
    onClose: () => void;
  }
  let { client, personID, personUIDs, project, target, onCreated, onClose }: Props = $props();
  const storagePrefix = 'msgvault.recurring-draft.';
  let storageKey = '';
  let defaultStorageKey = '';
  const empty = (): Draft => ({ key: '', attempted: false, project, target, title: '', body: '', list: 'agenda', owner: '', settings: { trigger_kind: 'cron', cron: '', interval_seconds: 60, timezone: 'UTC', occurrence_title: '', occurrence_body: '', recipient: '', catchup: '', grace_seconds: 0 } });
  let draft = $state<Draft>(empty());
  let initialized = $state(false);
  let review = $state(false);
  let submitting = $state(false);
  let notice = $state('');
  let storageBlocked = $state(true);
  let discardRequested = $state(false);
  let active = true;
  let releaseLock: (() => void) | undefined;
  let candidates = $state<{ key: string; title: string; attempted: boolean }[]>([]);

  function isDraft(value: unknown): value is Draft {
    if (!value || typeof value !== 'object') return false;
    const candidate = value as Record<string, unknown>;
    if (typeof candidate.key !== 'string' || typeof candidate.attempted !== 'boolean' ||
      ['title', 'body', 'list', 'owner'].some(key => typeof candidate[key] !== 'string') ||
      !candidate.settings || typeof candidate.settings !== 'object') return false;
    if (candidate.attempted && !candidate.key) return false;
    if (candidate.project !== undefined && typeof candidate.project !== 'string') return false;
    if (candidate.target !== undefined && typeof candidate.target !== 'string') return false;
    const settings = candidate.settings as Record<string, unknown>;
    return ['trigger_kind', 'cron', 'timezone', 'occurrence_title', 'occurrence_body', 'recipient', 'catchup'].every(key => typeof settings[key] === 'string') &&
      ['interval_seconds', 'grace_seconds'].every(key => typeof settings[key] === 'number');
  }

  function selectDraft(key: string): void {
    const stored = localStorage.getItem(key);
    const saved: unknown = stored === null ? null : JSON.parse(stored);
    if (!isDraft(saved)) throw new Error('Invalid recurring draft');
    storageKey = key;
    draft = saved;
    if (!draft.attempted) { draft.project = project; draft.target = target; }
    candidates = [];
    storageBlocked = false;
    initialized = true;
  }

  function discardUnattempted(key: string): void {
    try {
      const saved: unknown = JSON.parse(localStorage.getItem(key) ?? 'null');
      if (!isDraft(saved) || saved.attempted) throw new Error('Draft may have reached Kata');
      localStorage.removeItem(key);
      candidates = candidates.filter(candidate => candidate.key !== key);
      if (!candidates.length) {
        storageKey = defaultStorageKey;
        draft = empty();
        storageBlocked = false;
        initialized = true;
      }
    } catch { notice = 'The draft could not be discarded safely. Check Kata before starting over.'; }
  }

  function discardAttempted(): void {
    try {
      localStorage.removeItem(storageKey);
      draft = empty();
      discardRequested = false;
      review = false;
      notice = '';
    } catch { notice = 'The saved request could not be discarded. Check Kata before starting over.'; }
  }

  onMount(() => {
    if (!navigator.locks) {
      notice = 'This browser cannot safely coordinate recurring task drafts. Open this task in a supported browser.';
      return;
    }
    void navigator.locks.request(`${storagePrefix}lock`, { ifAvailable: true }, async (lock) => {
      if (!active) return;
      if (!lock) {
        notice = 'This recurring task draft is open in another tab. Close it there before continuing.';
        return;
      }
      try {
        const legacyKey = `${storagePrefix}${personID}`;
        if (!personUIDs.length || personUIDs.some(uid => !uid)) throw new Error('Missing person identity');
        const keys = [...new Set(personUIDs)].map(uid => `${storagePrefix}uid.${encodeURIComponent(uid)}`);
        defaultStorageKey = keys[0];
        const legacy = localStorage.getItem(legacyKey);
        const found = keys.filter(key => localStorage.getItem(key) !== null);
        if (legacy !== null) found.push(legacyKey);
        for (let index = 0; index < localStorage.length; index++) {
          const key = localStorage.key(index);
          if (!key || key === legacyKey || !/^msgvault\.recurring-draft\.\d+$/.test(key)) continue;
          const value = localStorage.getItem(key);
          const other: unknown = value === null ? null : JSON.parse(value);
          if (!isDraft(other) || other.attempted) throw new Error('Unresolved older draft');
        }
        if (found.length > 1) {
          candidates = found.map(key => {
            const value: unknown = JSON.parse(localStorage.getItem(key)!);
            if (!isDraft(value)) throw new Error('Invalid recurring draft');
            return { key, title: value.title || 'Untitled', attempted: value.attempted };
          });
          notice = 'Choose a saved draft. Unselected drafts remain saved.';
          await new Promise<void>((resolve) => { releaseLock = resolve; });
          return;
        }
        storageKey = found[0] ?? keys[0];
        if (storageKey === legacyKey) {
          const saved: unknown = JSON.parse(legacy!);
          if (!isDraft(saved)) throw new Error('Invalid recurring draft');
          localStorage.setItem(keys[0], legacy!);
          localStorage.removeItem(legacyKey);
          storageKey = keys[0];
        }
        const stored = localStorage.getItem(storageKey);
        if (stored) {
          const saved: unknown = JSON.parse(stored);
          if (!isDraft(saved)) throw new Error('Invalid recurring draft');
          draft = saved;
          if (!draft.attempted) { draft.project = project; draft.target = target; }
        }
        storageBlocked = false;
        initialized = true;
      } catch { notice = 'A saved draft or person identity could not be resolved safely. Check Kata and any older drafts before starting another request.'; }
      await new Promise<void>((resolve) => { releaseLock = resolve; });
    }).catch(() => {
      if (active) notice = 'This browser cannot safely coordinate recurring task drafts. Try again later.';
    });
  });
  $effect(() => {
    if (!initialized || storageBlocked) return;
    if (JSON.stringify(draft) === JSON.stringify(empty())) return;
    try { localStorage.setItem(storageKey, JSON.stringify(draft)); }
    catch { if (!notice) notice = 'The draft could not be saved on this device.'; }
  });
  onDestroy(() => {
    active = false;
    if (initialized && !storageBlocked && JSON.stringify(draft) !== JSON.stringify(empty())) {
      try { localStorage.setItem(storageKey, JSON.stringify(draft)); } catch { /* Keep any earlier saved request. */ }
    }
    releaseLock?.();
  });

  async function submit(): Promise<void> {
    if (submitting) return;
    const wasAttempted = draft.attempted;
    if (wasAttempted && (!draft.project || draft.project !== project || !draft.target || draft.target !== target)) {
      notice = 'The original Kata project is unavailable or changed. Check Kata before starting over.';
      return;
    }
    const request = { project: draft.project, target: draft.target, title: draft.title, body: draft.body, list: draft.list, owner: draft.owner, settings: draft.settings };
    if (!wasAttempted && new TextEncoder().encode(JSON.stringify(request)).length > 64 * 1024) {
      notice = 'This recurring task is too large. Shorten the notes or other fields and try again.';
      return;
    }
    if (!draft.key) draft.key = `${new Date().toISOString()}/${crypto.randomUUID()}`;
    draft.attempted = true;
    // Persist the identity before either remote write can begin.
    try { localStorage.setItem(storageKey, JSON.stringify(draft)); }
    catch {
      if (!wasAttempted) {
        draft.attempted = false;
        draft.key = '';
      }
      notice = wasAttempted ? 'The draft could not be saved. An earlier request may have reached Kata; check Kata before discarding.' : 'The draft could not be saved on this device. No schedule was created.';
      return;
    }
    submitting = true;
    notice = '';
    try {
      const result = await orvalFetch<{ parent: PersonAgendaItem; schedule?: PersonAgendaSchedule; status: string }>({
        url: `/api/v1/people/${personID}/agenda/recurring`, method: 'POST',
        headers: { 'Content-Type': 'application/json', 'Idempotency-Key': draft.key },
        data: request,
      }, client);
      if (!active) return;
      if (!result.data) {
        if (!wasAttempted && result.response.status === 422 && (result.error?.error === 'invalid_schedule' || result.error?.error === 'cron_unsupported' || result.error?.error === 'invalid_idempotency_key')) {
          draft.attempted = false;
          draft.key = '';
        }
        notice = result.error?.message ?? 'The outcome is unknown. Retry this same request or check Kata.';
        return;
      }
      if (!result.data.schedule) {
        if (result.data.status === 'forbidden') notice = 'The parent exists, but schedule access is forbidden. Check Kata before retrying.';
        else if (result.data.status === 'authentication_required') notice = 'The parent exists, but Kata authentication is required. Retry this same request after reconnecting.';
        else if (result.data.status === 'conflict') notice = 'The schedule conflicts with the saved request. Check Kata before starting another.';
        else notice = 'The parent exists, but the schedule is not confirmed. Retry this same request or check Kata.';
        return;
      }
      initialized = false;
      try { localStorage.removeItem(storageKey); } catch { /* The confirmed result remains valid. */ }
      onCreated(result.data.parent, result.data.schedule);
    } catch {
      if (active) notice = 'The outcome is unknown. Retry this same request or check Kata.';
    } finally {
      if (active) submitting = false;
    }
  }
</script>

<div role="group" aria-label="New recurring task">
  {#if storageBlocked}
    <p role="status">{notice}</p>
    {#each candidates as candidate (candidate.key)}
      <Button size="sm" label={`${candidate.attempted ? 'Resume' : 'Open'} draft: ${candidate.title}`} onclick={() => { try { selectDraft(candidate.key); } catch { candidates = []; notice = 'The selected draft could not be loaded. Check Kata before starting over.'; } }} />
      {#if !candidate.attempted}<Button size="sm" label={`Discard draft: ${candidate.title}`} onclick={() => discardUnattempted(candidate.key)} />{/if}
    {/each}
    <Button size="sm" label="Close recurring task" onclick={onClose} />
  {:else}
  <p>Kata creates child tasks and sends inbox requests. It does not message this person.</p>
  <TextInput bind:value={draft.title} ariaLabel="Recurring task title" disabled={draft.attempted || submitting} />
  <label>Parent task notes <textarea bind:value={draft.body} disabled={draft.attempted || submitting}></textarea></label>
  <TextInput bind:value={draft.list} ariaLabel="Recurring task list" disabled={draft.attempted || submitting} />
  <TextInput bind:value={draft.owner} ariaLabel="Parent task owner" disabled={draft.attempted || submitting} />
  <label>Cadence <select aria-label="Recurring cadence" bind:value={draft.settings.trigger_kind} disabled={draft.attempted || submitting}><option value="cron">Calendar schedule</option><option value="interval">Elapsed interval</option></select></label>
  {#if draft.settings.trigger_kind === 'cron'}
    <label>Common cadence <select aria-label="Common cadence" disabled={draft.attempted || submitting} onchange={(event) => { const value = event.currentTarget.value; if (value) draft.settings.cron = value; }}><option value="">Custom</option><option value="0 9 * * *">Daily at 09:00</option><option value="0 9 * * 1">Monday at 09:00</option><option value="0 9 1 * *">First of month at 09:00</option></select></label>
    <TextInput bind:value={draft.settings.cron} ariaLabel="Recurring cron expression" disabled={draft.attempted || submitting} />
  {:else}<label>Interval seconds <NumberInput ariaLabel="Interval seconds" min={60} value={draft.settings.interval_seconds} oninput={(value) => { draft.settings.interval_seconds = value ?? 0; }} disabled={draft.attempted || submitting} /></label>{/if}
  <TextInput bind:value={draft.settings.timezone} ariaLabel="Recurring timezone" disabled={draft.attempted || submitting} />
  <TextInput bind:value={draft.settings.occurrence_title} ariaLabel="Recurring occurrence title" disabled={draft.attempted || submitting} />
  <label>Occurrence body <textarea bind:value={draft.settings.occurrence_body} disabled={draft.attempted || submitting}></textarea></label>
  <TextInput bind:value={draft.settings.recipient} ariaLabel="Recurring inbox recipient" disabled={draft.attempted || submitting} />
  <label>Missed occurrences <select aria-label="Recurring missed occurrences" bind:value={draft.settings.catchup} disabled={draft.attempted || submitting}><option value="">Choose a policy</option><option value="all">Every missed occurrence</option><option value="latest">Only the latest</option><option value="skip">Skip old occurrences</option></select></label>
  {#if draft.settings.catchup === 'skip'}<label>Max age (s) <NumberInput ariaLabel="Max age (s)" min={1} value={draft.settings.grace_seconds} oninput={(value) => { draft.settings.grace_seconds = value ?? 0; }} disabled={draft.attempted || submitting} /></label>{/if}
  <p>Calendar schedules use the selected timezone; daylight-saving changes can skip or repeat local times. Keep Kata running in the background for delivery.</p>
  {#if notice}<p role="status">{notice}</p>{/if}
  {#if !draft.attempted}<Button size="sm" label="Review recurring task" disabled={submitting} onclick={() => { review = true; }} />{/if}
  {#if review || draft.attempted}
    <Button size="sm" label={draft.attempted ? 'Retry recurring task' : 'Create recurring task'} disabled={submitting || !draft.title.trim() || !draft.owner.trim() || !draft.settings.occurrence_title.trim() || !draft.settings.recipient.trim() || !draft.settings.catchup} onclick={() => void submit()} />
  {/if}
  {#if draft.attempted}
    {#if discardRequested}
      <p>Check Kata first. Discarding this request and starting another could create a duplicate.</p>
      <Button size="sm" label="Discard saved request" disabled={submitting} onclick={discardAttempted} />
      <Button size="sm" label="Keep saved request" disabled={submitting} onclick={() => { discardRequested = false; }} />
    {:else}
      <Button size="sm" label="Start over" disabled={submitting} onclick={() => { discardRequested = true; }} />
    {/if}
  {/if}
  <Button size="sm" label="Close recurring task" disabled={submitting} onclick={onClose} />
  {/if}
</div>
