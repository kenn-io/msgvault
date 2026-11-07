import { getParticipant, getPersonProfile } from '../api/generated/api/api';
import type { APIClient } from '../api/client';
import type { PersonMergeDetail } from '../api/generated/models';
import { identityChipText, type IdentityChipText } from '../relationships/identity-chip';

export async function loadMergeLabels(client: APIClient, detail: PersonMergeDetail, signal: AbortSignal) {
  const names = new Map<number, string>();
  const addresses = new Map<number, IdentityChipText[]>();
  let incomplete = false;
  const options = { ...client, signal };
  const personIDs = new Set([detail.merge.survivor_person_id, ...(detail.splits ?? []).map(split => split.new_person_id)]);
  const results = await Promise.allSettled([
    ...[...personIDs].map(async id => {
      const response = await getPersonProfile({ id }, options);
      if (response.data) {
        const name = response.data.display_name?.trim();
        if (name) names.set(id, name);
      } else if (response.response.status !== 404) incomplete = true;
    }),
    ...(detail.participants ?? []).map(async ({ participant_id: id }) => {
      const response = await getParticipant({ id }, options);
      if (!response.data) { incomplete = true; return; }
      const member = response.data.cluster?.members?.find(member => member.participant_id === id);
      const identifiers = (response.data.identifiers ?? []).filter(identifier => identifier.participant_id === id);
      addresses.set(id, identifiers.length
        ? identifiers.map(identifier => identityChipText(identifier, member, []))
        : member ? [identityChipText(undefined, member, [])] : response.data.display_name?.trim() ? [{ title: response.data.display_name.trim(), subtitle: '', detail: '' }] : []);
    }),
  ]);
  incomplete ||= results.some(result => result.status === 'rejected');
  const ambiguousLabels: { id: number; label: IdentityChipText }[] = [];
  for (const [id, labels] of addresses) {
    for (const label of labels) {
      if ([...addresses].some(([otherID, others]) => otherID !== id && others.some(other =>
        other.title === label.title && other.subtitle === label.subtitle && other.detail === label.detail))) {
        ambiguousLabels.push({ id, label });
      }
    }
  }
  for (const { id, label } of ambiguousLabels) {
    label.detail = [label.detail, `Participant ${id}`].filter(Boolean).join(' · ');
  }
  return { names, addresses, incomplete };
}
