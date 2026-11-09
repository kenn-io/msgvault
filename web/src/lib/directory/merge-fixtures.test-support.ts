import type { ParticipantAddress } from '../api/generated/models';

export function addressFixture(participant_id: number, type: string, value: string, fields: Partial<ParticipantAddress> = {}): ParticipantAddress {
  return { participant_id, type, value, display_value: '', normalized_value: value, service_slug: type, service_label: type, scope_kind: '', scope_value: '', participant_display_name: '', identity_key: fields.normalized_value ?? value, ...fields };
}
