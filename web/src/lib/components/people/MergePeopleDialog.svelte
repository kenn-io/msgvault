<script lang="ts">
  import { createPerson, getPersonProfile } from '../../api/generated/api/api';
  import type { APIClient } from '../../api/client';
  import type { RelationshipsController, RelationshipsMergeContext, LinkOutcome } from '../../relationships/controller.svelte';
  import { validatePersonMergeRequired, type ValidatedPersonMergeRequired, type PersonMergeSuccess } from '../../directory/person-merge';
  import LinkIdentityDialog from '../relationships/LinkIdentityDialog.svelte';
  import PersonBindingConflictModal from '../directory/PersonBindingConflictModal.svelte';
  import { untrack } from 'svelte';
  let { client, controller, personID: currentPersonID, participantID: currentParticipantID, name, onReconcile, onOpenPerson, onLinked, onClose }:
    { client: APIClient; controller: RelationshipsController; personID: number | null; participantID: number | null; name: string;
      onReconcile: (context: RelationshipsMergeContext, participantsLinked?: boolean) => Promise<void>; onOpenPerson?: (id: number) => void; onLinked?: (a: number, b: number, outcome: LinkOutcome) => void; onClose: () => void } = $props();
  let conflict = $state<ValidatedPersonMergeRequired>();
  // The operation keeps the people it opened with, even after navigation disposes the dialog.
  const [personID, participantID] = untrack(() => [currentPersonID, currentParticipantID]);
  const context = untrack(() => controller.personMergeContextSnapshot());
  // A successful promotion is retained when reading the profiles needs a retry.
  const promoted = new Map<number, number>();
  async function savedPair(a: number, b: number): Promise<LinkOutcome> {
    try {
      const profiles = await Promise.all([a, b].map(id => getPersonProfile({ id }, client)));
      const choice = validatePersonMergeRequired({ error: 'person_merge_required', message: 'Choose which person to keep.', profiles: profiles.map(result => ({ person: result.data, etag: result.response.headers.get('ETag') })) });
      return choice ? { ok: false, code: 'merge_required', message: choice.message, conflict: choice } : { ok: false, code: 'error', message: 'Could not read both people. Retry the merge.' };
    } catch { return { ok: false, code: 'error', message: 'Could not read both people. Retry the merge.' }; }
  }
  async function link(other: number): Promise<LinkOutcome> {
    if (participantID !== null) { const outcome = await controller.linkParticipants(participantID, other); onLinked?.(participantID, other, outcome); return outcome; }
    if (personID === null) return { ok: false, code: 'error', message: 'Choose a person first.' };
    try {
      const profile = await getPersonProfile({ id: personID }, client);
      if (!profile.data) return { ok: false, code: 'error', message: profile.error?.message ?? 'Could not read this person.' };
      const representative = profile.data.participant_ids?.[0];
      if (representative !== undefined) { const outcome = await controller.linkParticipants(representative, other); onLinked?.(representative, other, outcome); return outcome; }
      let id = promoted.get(other);
      if (!id) {
        const result = await createPerson({ participant_id: other }, client);
        if (!result.data) return { ok: false, code: 'error', message: result.error?.message ?? 'Could not save the selected person.' };
        id = result.data.id;
        promoted.set(other, id);
      }
      return savedPair(personID, id);
    } catch { return { ok: false, code: 'error', message: 'Could not save the selected person.' }; }
  }
  async function pickSaved(other: number): Promise<LinkOutcome> {
    if (personID !== null) return savedPair(personID, other);
    try {
      const result = await getPersonProfile({ id: other }, client);
      if (!result.data) return { ok: false, code: 'error', message: result.error?.message ?? 'Could not read the selected person.' };
      const ids = result.data.participant_ids as number[] | undefined;
      if (!ids?.length && participantID !== null) {
        let id = promoted.get(participantID);
        if (!id) { const saved = await createPerson({ participant_id: participantID }, client); if (!saved.data) return { ok: false, code: 'error', message: saved.error?.message ?? 'Could not save this person.' }; id = saved.data.id; promoted.set(participantID, id); }
        return savedPair(other, id);
      }
      if (!ids?.length) return { ok: false, code: 'error', message: 'This person has no addresses.' };
      return link(ids[0]!);
    } catch { return { ok: false, code: 'error', message: 'Could not read the selected person.' }; }
  }
  async function merged(success: PersonMergeSuccess): Promise<void> {
    onClose();
    onOpenPerson?.(success.survivor.id);
    await onReconcile(context);
  }
</script>
{#if conflict}
  <PersonBindingConflictModal {client} {conflict} onOpenProfile={(id) => { onClose(); onOpenPerson?.(id); }} onSuccess={merged} {onClose} />
{:else}
  <LinkIdentityDialog {client} excludeID={participantID ?? 0} excludePersonID={personID ?? undefined} personLabel={name} onConfirm={async (id) => { const result = await link(id); if (result.ok || promoted.size > 0) await onReconcile(context, result.ok); return result; }} onConfirmPerson={async (id) => { const result = await pickSaved(id); if (result.ok || promoted.size > 0) await onReconcile(context, result.ok); return result; }} onMergeRequired={(value) => conflict = value} {onClose} />
{/if}
