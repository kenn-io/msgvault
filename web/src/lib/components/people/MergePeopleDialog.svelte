<script lang="ts">
  import { createPerson, getPersonProfile } from '../../api/generated/api/api';
  import type { APIClient } from '../../api/client';
  import type { RelationshipsController, RelationshipsMergeContext, LinkOutcome } from '../../relationships/controller.svelte';
  import { validatePersonMergeRequired, type ValidatedPersonMergeRequired, type PersonMergeSuccess } from '../../directory/person-merge';
  import LinkIdentityDialog from '../relationships/LinkIdentityDialog.svelte';
  import PersonBindingConflictModal from '../directory/PersonBindingConflictModal.svelte';
  import { onDestroy, untrack } from 'svelte';
  import { showFlash } from '@kenn-io/kit-ui';
  let { client, controller, personID: currentPersonID, participantID: currentParticipantID, name, onReconcile, onOpenPerson, onLinked, onClose }:
    { client: APIClient; controller: RelationshipsController; personID: number | null; participantID: number | null; name: string;
      onReconcile: (context: RelationshipsMergeContext, participantsLinked?: boolean, success?: PersonMergeSuccess) => Promise<void>; onOpenPerson?: (id: number) => void; onLinked?: (a: number, b: number, outcome: LinkOutcome) => void; onClose: () => void } = $props();
  let conflict = $state<ValidatedPersonMergeRequired>();
  // The operation keeps the people it opened with, even after navigation disposes the dialog.
  const [personID, participantID] = untrack(() => [currentPersonID, currentParticipantID]);
  const context = untrack(() => controller.personMergeContextSnapshot());
  // A late completion after navigation belongs to this closed dialog, not whichever one replaced it.
  let disposed = false;
  onDestroy(() => disposed = true);
  const close = () => { if (!disposed) onClose(); };
  let promoted = false;
  async function savedPair(a: number, b: number): Promise<LinkOutcome> {
    try {
      const profiles = await Promise.all([a, b].map(id => getPersonProfile({ id }, client)));
      const choice = validatePersonMergeRequired({ error: 'person_merge_required', message: 'Choose which person to keep.', profiles: profiles.map(result => ({ person: result.data, etag: result.response.headers.get('ETag') })) });
      return choice ? { ok: false, code: 'merge_required', message: choice.message, conflict: choice } : { ok: false, code: 'error', message: "Couldn't load both people." };
    } catch { return { ok: false, code: 'error', message: "Couldn't load both people." }; }
  }
  async function link(other: number): Promise<LinkOutcome> {
    if (participantID !== null) { const outcome = await controller.linkParticipants(participantID, other); if (!disposed) onLinked?.(participantID, other, outcome); return outcome; }
    if (personID === null) return { ok: false, code: 'error', message: 'Choose a person first.' };
    try {
      const profile = await getPersonProfile({ id: personID }, client);
      if (!profile.data) return { ok: false, code: 'error', message: profile.error?.message ?? "Couldn't load this person." };
      const representative = profile.data.participant_ids?.[0];
      if (representative !== undefined) { const outcome = await controller.linkParticipants(representative, other); if (!disposed) onLinked?.(representative, other, outcome); return outcome; }
      const result = await createPerson({ participant_id: other }, client);
      if (!result.data) return { ok: false, code: 'error', message: result.error?.message ?? "Couldn't save the chosen person." };
      promoted = true;
      return savedPair(personID, result.data.id);
    } catch { return { ok: false, code: 'error', message: "Couldn't save the chosen person." }; }
  }
  async function pickSaved(other: number): Promise<LinkOutcome> {
    if (personID !== null) return savedPair(personID, other);
    try {
      const result = await getPersonProfile({ id: other }, client);
      if (!result.data) return { ok: false, code: 'error', message: result.error?.message ?? "Couldn't load the chosen person." };
      const ids = result.data.participant_ids as number[] | undefined;
      if (!ids?.length && participantID !== null) {
        const saved = await createPerson({ participant_id: participantID }, client); if (!saved.data) return { ok: false, code: 'error', message: saved.error?.message ?? "Couldn't save this person." };
        promoted = true;
        return savedPair(other, saved.data.id);
      }
      if (!ids?.length) return { ok: false, code: 'error', message: 'This person has no addresses.' };
      return link(ids[0]!);
    } catch { return { ok: false, code: 'error', message: "Couldn't load the chosen person." }; }
  }
  async function merged(success: PersonMergeSuccess): Promise<void> {
    close(); if (success.result.cache_state === 'stale') showFlash("People merged. Couldn't refresh the people list.", { tone: 'warning', durationMs: 8000 });
    const reconciliation = onReconcile(context, false, success);
    onOpenPerson?.(success.survivor.id);
    await reconciliation;
  }
</script>
{#if conflict}
  <PersonBindingConflictModal {client} {conflict} onOpenProfile={(id) => { close(); onOpenPerson?.(id); }} onSuccess={merged} onClose={close} />
{:else}
  <LinkIdentityDialog {client} excludeID={participantID ?? 0} excludePersonID={personID} personLabel={name} onConfirm={async (id) => { const result = await link(id); if (result.ok || promoted) await onReconcile(context, result.ok); return result; }} onConfirmPerson={async (id) => { const result = await pickSaved(id); if (result.ok || promoted) await onReconcile(context, result.ok); return result; }} onMergeRequired={(value) => conflict = value} onClose={close} />
{/if}
