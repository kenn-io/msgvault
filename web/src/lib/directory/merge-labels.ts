import type { PersonMergeDetail, PersonMergeLabels, ParticipantAddress } from '../api/generated/models';
import { identityChipText } from '../relationships/identity-chip';

export interface AddressIdentity { identity_key: string; service_slug?: string; scope_kind?: string; scope_value?: string }
type AddressLabel = AddressIdentity & { title: string; detail: string };
export function addressText(address: { title: string; detail?: string } | undefined): string | undefined {
  return address ? [address.title, address.detail].filter(Boolean).join(' · ') : undefined;
}
export interface MergeLabels { names: Map<number, string>; addresses: Map<number, AddressLabel[]>; incomplete: boolean }
export interface MergeRow extends MergeLabels { count: number; absorbedAddress?: string }
export interface MergeAddress extends AddressIdentity { id: string; participantID?: number; title: string; value: string; detail?: string; splitID?: number }
export interface MergedInAddress extends AddressIdentity { mergeID: number; participantID: number; value: string; mergedAt: string }

/**
 * "{absorbed} merged into {survivor}". When one name contains the other as whole
 * words, as with "Mira Vale · work" and "Mira Vale", or the absorbed profile has no name,
 * it is named by its first address so the two read apart.
 */
export function mergeSentence(absorbed: string | undefined, survivor: string | undefined, absorbedAddress?: string): string {
  return `${absorbedCardTitle(absorbed, survivor, absorbedAddress)} merged into ${survivor ?? 'this profile'}`;
}

/**
 * The absorbed profile's name on the merge's title and card: its name, or its
 * first address when the names read alike or it has none. "Merged-in profile"
 * only when it has neither.
 */
export function absorbedCardTitle(absorbed: string | undefined, survivor: string | undefined, firstAddress?: string): string {
  return !absorbed || (!!survivor && sameName(absorbed, survivor)) ? firstAddress ?? absorbed ?? 'Merged-in profile' : absorbed;
}

/** A profile's entries without the one its name reads as, since the name stands for it; another that reads the same still lists. */
export function withoutName<T>(name: string, items: T[], text: (item: T) => string | undefined = String): T[] {
  const named = items.findIndex(item => text(item) === name);
  return items.filter((_, index) => index !== named);
}

/** Whether one name holds the other as whole words: Mira within "Mira Vale", but not Ann within "Joanna". */
function sameName(left: string, right: string): boolean {
  const a = left.trim().toLocaleLowerCase(), b = right.trim().toLocaleLowerCase();
  return !!a && !!b && (containsWords(a, b) || containsWords(b, a));
}

function containsWords(text: string, words: string): boolean {
  const escaped = words.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
  return new RegExp(`(?<![\\p{L}\\p{M}\\p{N}])${escaped}(?![\\p{L}\\p{M}\\p{N}])`, 'u').test(text);
}

/** The addresses one side's participants hold, in participant order, with the split that took each. */
export function mergeAddresses(detail: MergeSides, side: 'absorbed' | 'survivor', workspaces: Map<string, string>, name = ''): MergeAddress[] {
  const current = participantAddressMap(detail.labels, workspaces, name);
  return withSavedAddresses(detail, side, (detail.participants ?? []).filter(participant => participant.origin_side === side).flatMap(participant =>
    (current.get(participant.participant_id) ?? []).map((address, index) => ({
      ...address, id: `${participant.participant_id}-${index}`, participantID: participant.participant_id,
      value: address.title, detail: address.detail || undefined, splitID: participant.split_id,
    }))), workspaces);
}

type MergeSides = Pick<PersonMergeDetail, 'participants'> & Partial<Pick<PersonMergeDetail, 'merge' | 'labels'>>;

/**
 * Adds the addresses saved on the profile itself, by its editor or an import, which the merge moved with it, once each.
 * An unserviced address is an ordinary email, phone, or username; one on WhatsApp, Slack, or a scope names them under
 * it by the service's registered label. Language is profile metadata, not an address.
 */
function withSavedAddresses(detail: MergeSides, side: 'absorbed' | 'survivor', addresses: MergeAddress[], workspaces: Map<string, string>): MergeAddress[] {
  const personID = side === 'absorbed' ? detail.merge?.absorbed_person_id : detail.merge?.survivor_person_id;
  const seen = new Set(addresses.map(address => address.identity_key));
  const points = (detail.labels?.profiles.find(profile => profile.id === personID)?.contact_points ?? [])
    .filter(point => point.address_kind !== 'language' && !seen.has(point.identity_key) && !!seen.add(point.identity_key));
  return addresses.concat(points.map((point, index) => {
    const service = point.service_slug || point.address_kind, scope_kind = point.scope_kind || '', scope_value = point.scope_value || '';
    const scope = scopeText({ service_slug: service, scope_kind, scope_value }, workspaces);
    const detail = [service === point.address_kind ? '' : point.service_label, scope].filter(Boolean).join(' · ') || undefined;
    return { identity_key: point.identity_key, id: `saved-${personID}-${index}`, title: point.original_value, value: point.original_value, detail, service_slug: service, scope_kind, scope_value };
  }));
}

/** The server supplies recorded names and canonical identities for a page or selected merge. */
export function mergeSummaryRow(item: { merge: PersonMergeDetail['merge']; labels: PersonMergeLabels; participants?: PersonMergeDetail['participants'] }, workspaces: Map<string, string>): MergeRow {
  const labels = item.labels;
  const participants = item.participants ?? labels.participants ?? [];
  const names = new Map(labels.profiles.flatMap(profile => profile.display_name ? [[profile.id, profile.display_name] as const] : []));
  const addresses = participantAddressMap(labels, workspaces);
  const rows = mergeAddresses({ participants, merge: item.merge, labels }, 'absorbed', workspaces);
  return {
    names, addresses, incomplete: labels.incomplete || participants.some(participant => !addresses.has(participant.participant_id)),
    count: labels.absorbed_address_count, absorbedAddress: addressText(rows[0]),
  };
}

function participantAddressMap(labels: PersonMergeLabels | undefined, workspaces: Map<string, string>, name = ''): Map<number, AddressLabel[]> {
  return new Map((labels?.participant_ids ?? []).map(id => [id, (labels?.addresses ?? []).filter(address => address.participant_id === id).map(address => participantAddress(address, workspaces, name))]));
}

function participantAddress(address: ParticipantAddress, workspaces: Map<string, string>, name = ''): AddressLabel {
  const chip = identityChipText({ ...address, is_primary: false, provenance: '', scope_kind: '', scope_value: '' }, undefined, []);
  const detail = ['email', 'phone'].includes(address.type) ? '' : [...new Set([
    ...withoutName(name, [chip.subtitle]),
    ['handle', 'matrix', 'apple_id', 'imessage', 'whatsapp', 'sms', 'synctech_sms', 'google_voice'].includes(address.type) ? chip.detail : address.display_value,
    scopeText(address, workspaces),
  ].filter(Boolean))].join(' · ');
  return { ...address, title: chip.title, detail };
}

/** Exact scope keys stay in identities; existing labels remain stable as scopes arrive. */
export function workspaceLabels(addresses: Omit<AddressIdentity, 'identity_key'>[], known = new Map<string, string>()): Map<string, string> {
  const labels = new Map(known);
  const scopes = addresses.filter(address => address.scope_value).map(address => JSON.stringify([address.scope_kind || 'scope', address.scope_value]));
  for (const scope of [...new Set(scopes)].filter(scope => !labels.has(scope)).sort()) {
    const [kind] = JSON.parse(scope) as [string, string];
    labels.set(scope, `${kind} ${[...labels.keys()].filter(key => (JSON.parse(key) as string[])[0] === kind).length + 1}`);
  }
  return labels;
}

export function scopeText(identity: Omit<AddressIdentity, 'identity_key'>, workspaces: Map<string, string>): string {
  return identity.scope_value ? workspaces.get(JSON.stringify([identity.scope_kind || 'scope', identity.scope_value])) ?? '' : identity.scope_kind ?? '';
}

/** One label per participant, built over the whole set; labels that read alike gain "(n)" in participant-ID order. */
export function distinctLabels(ids: number[], label: (id: number) => string): Map<number, string> {
  const texts = [...new Set(ids)].sort((left, right) => left - right).map(id => [id, label(id)] as const);
  const seen = new Map<string, number>();
  return new Map(texts.map(([id, text]) => {
    if (texts.filter(other => other[1] === text).length < 2) return [id, text];
    seen.set(text, (seen.get(text) ?? 0) + 1);
    return [id, `${text} (${seen.get(text)})`];
  }));
}

interface MergedInParticipant { merge_id: number; participant_id: number; merged_at: string; addresses: ParticipantAddress[] }

/** The addresses each current member brought in with its newest merge. */
export function mergedInAddresses(records: MergedInParticipant[], workspaces: Map<string, string>): MergedInAddress[] {
  return records.flatMap(record => record.addresses.map(value => participantAddress(value, workspaces)).map(address => ({
    ...address, mergeID: record.merge_id, participantID: record.participant_id,
    mergedAt: record.merged_at, value: addressText(address)!,
  })));
}
