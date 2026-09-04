<script lang="ts">
  import {
    appShortcuts,
    Button,
    Checkbox,
    Modal,
    SegmentedControl,
    type SegmentedControlOption
  } from '@kenn-io/kit-ui';
  import { onDestroy, onMount, untrack } from 'svelte';

  import type { APIClient } from '../../api/client';
  import {
    isMatchingPersonETag,
    isPersonMergeRevisionConflict,
    validatePersonMergeRequired,
    type PersonMergeSuccess,
    type ValidatedPersonMergeRequired
  } from '../../directory/person-merge';

  type Profile = ValidatedPersonMergeRequired['profiles'][number];

  interface Props {
    client: APIClient;
    conflict: ValidatedPersonMergeRequired;
    onOpenProfile: (personID: number) => void;
    onSuccess: (success: PersonMergeSuccess) => void | Promise<void>;
    onClose: () => void;
  }

  let { client, conflict, onOpenProfile, onSuccess, onClose }: Props = $props();
  let profiles = $state<[Profile, Profile]>(untrack(() => [conflict.profiles[0], conflict.profiles[1]]));
  let survivorID = $state<number | null>(null);
  let confirmed = $state(false);
  let pending = $state(false);
  let completed = $state(false);
  let error = $state<string | null>(null);
  let idempotencyKey = $state<string | null>(null);
  let reloadRequired = $state(false);
  let requestGeneration = 0;
  let disposed = false;
  let abortController: AbortController | undefined;
  let releaseShortcutScope: (() => void) | undefined;

  const options = $derived<SegmentedControlOption[]>(profiles.map((profile) => ({
    value: String(profile.person.id),
    label: profileLabel(profile)
  })));

  onMount(() => {
    releaseShortcutScope = appShortcuts.pushScope('person-binding-conflict-modal');
  });

  onDestroy(() => {
    disposed = true;
    requestGeneration += 1;
    abortController?.abort();
    releaseShortcutScope?.();
  });

  function profileName(profile: Profile): string {
    return profile.person.display_name?.trim() || `Person ${profile.person.id}`;
  }

  function profileLabel(profile: Profile): string {
    return `${profileName(profile)} (Person ${profile.person.id})`;
  }

  function selectSurvivor(value: string): void {
    const nextID = Number(value);
    if (nextID === survivorID) return;
    survivorID = nextID;
    confirmed = false;
    idempotencyKey = null;
    error = null;
  }

  function requestClose(): void {
    if (pending || completed) return;
    onClose();
  }

  async function reloadProfiles(generation: number, signal: AbortSignal): Promise<void> {
    try {
      const requestedProfiles = profiles;
      const results = await Promise.all(requestedProfiles.map((profile) => client.GET('/api/v1/people/{id}', {
        params: { path: { id: profile.person.id } },
        signal
      })));
      if (disposed || generation !== requestGeneration) return;

      const refreshed = results.map((result, index) => {
        const expectedPersonID = requestedProfiles[index]!.person.id;
        const etag = result.response.headers.get('ETag');
        if (!result.data || result.data.id !== expectedPersonID || !isMatchingPersonETag(etag, expectedPersonID)) return null;
        return { person: result.data, etag };
      });
      if (!refreshed[0] || !refreshed[1]) {
        error = 'The merge was stale, but could not load both current profile revisions. Try again.';
        return;
      }
      const next = validatePersonMergeRequired({
        error: 'person_merge_required',
        message: conflict.message,
        profiles: refreshed
      });
      if (!next) {
        error = 'The merge was stale, but could not load both current profile revisions. Try again.';
        return;
      }
      profiles = next.profiles;
      survivorID = null;
      confirmed = false;
      idempotencyKey = null;
      reloadRequired = false;
      error = 'Profiles changed while you were reviewing them. Confirm the current revisions before merging.';
    } catch (cause) {
      if (disposed || generation !== requestGeneration || signal.aborted) return;
      error = 'The merge was stale, but could not load both current profile revisions. Try again.';
    }
  }

  async function retryProfileReload(): Promise<void> {
    if (pending || completed || !reloadRequired) return;
    pending = true;
    error = null;
    abortController = new AbortController();
    const generation = ++requestGeneration;

    try {
      await reloadProfiles(generation, abortController.signal);
    } finally {
      if (!disposed && generation === requestGeneration) pending = false;
    }
  }

  async function submit(): Promise<void> {
    if (pending || completed || reloadRequired || !confirmed || survivorID === null) return;
    const survivor = profiles.find((profile) => profile.person.id === survivorID);
    const absorbed = profiles.find((profile) => profile.person.id !== survivorID);
    if (!survivor || !absorbed) return;

    const key = idempotencyKey ?? crypto.randomUUID();
    idempotencyKey = key;
    pending = true;
    error = null;
    abortController = new AbortController();
    const generation = ++requestGeneration;

    try {
      const response = await client.POST('/api/v1/people/{id}/merge', {
        params: {
          path: { id: survivor.person.id },
          header: {
            'If-Match': `${survivor.etag}, ${absorbed.etag}`,
            'Idempotency-Key': key
          }
        },
        body: { absorbed_person_id: absorbed.person.id },
        signal: abortController.signal
      });
      if (disposed || generation !== requestGeneration) return;

      if (response.data) {
        completed = true;
        const responseETag = response.response.headers.get('ETag');
        try {
          await onSuccess({
            result: response.data,
            survivor: response.data.person,
            responseETag: isMatchingPersonETag(responseETag, response.data.person.id) ? responseETag : null
          });
        } catch {
          error = 'People were merged, but the surrounding view could not be refreshed.';
        }
        return;
      }

      confirmed = false;
      idempotencyKey = null;
      if (isPersonMergeRevisionConflict(response.error)) {
        reloadRequired = true;
        await reloadProfiles(generation, abortController.signal);
      } else {
        error = response.error?.message ?? 'The people could not be merged.';
      }
    } catch (cause) {
      if (disposed || generation !== requestGeneration || abortController.signal.aborted) return;
      error = cause instanceof Error ? cause.message : 'The merge request could not be sent.';
    } finally {
      if (!disposed && generation === requestGeneration) pending = false;
    }
  }
</script>

<Modal
  title="Resolve person merge"
  ariaLabel="Resolve person merge"
  closeLabel="Close person merge"
  closable={!pending && !completed}
  closeOnOverlayClick={!pending && !completed}
  onclose={requestClose}
  maxWidth="min(680px, calc(100vw - 32px))"
>
  <div class="conflict" aria-busy={pending}>
    <p>These identities already belong to different durable profiles. Inspect both profiles, then choose the one that survives.</p>

    <div class="profiles">
      {#each profiles as profile (profile.person.id)}
        <article>
          <strong>{profileName(profile)}</strong>
          <span>Person {profile.person.id}, revision {profile.person.revision}</span>
          <Button
            surface="soft"
            label={`Open ${profileName(profile)} profile`}
            disabled={pending}
            onclick={() => onOpenProfile(profile.person.id)}
          />
        </article>
      {/each}
    </div>

    <SegmentedControl
      ariaLabel="Merge survivor"
      {options}
      value={survivorID === null ? '' : String(survivorID)}
      onchange={selectSurvivor}
      disabled={pending || completed || reloadRequired}
      block
    />

    <Checkbox
      checked={confirmed}
      label="I understand this consolidates both profiles into the selected survivor."
      disabled={survivorID === null || pending || completed || reloadRequired}
      onchange={(checked) => { confirmed = checked; }}
    />

    {#if error}<p class="error" role="alert">{error}</p>{/if}
    {#if completed}<p role="status">People merged.</p>{/if}
  </div>

  {#snippet footer()}
    <Button surface="soft" label="Cancel" disabled={pending || completed} onclick={requestClose} />
    {#if !completed}
      {#if reloadRequired}
        <Button
          surface="soft"
          label="Retry profile reload"
          disabled={pending}
          onclick={() => void retryProfileReload()}
        />
      {/if}
      <Button
        tone="info"
        surface="solid"
        label="Merge into selected survivor"
        disabled={pending || reloadRequired || survivorID === null || !confirmed}
        onclick={() => void submit()}
      />
    {/if}
  {/snippet}
</Modal>

<style>
  .conflict { display: grid; gap: var(--space-4); min-width: min(34rem, calc(100vw - 64px)); }
  p { margin: 0; }
  .profiles { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: var(--space-3); }
  article { display: grid; align-content: start; gap: var(--space-2); padding: var(--space-4); border: var(--border-width) solid var(--border-default); border-radius: var(--radius-sm); background: var(--bg-inset); }
  article span { color: var(--text-muted); font-size: var(--font-size-sm); }
  .error { color: var(--text-danger); }
  @media (max-width: 640px) { .profiles { grid-template-columns: 1fr; } }
</style>
