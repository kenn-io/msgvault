import type { ParticipantAddress } from '../api/generated/models';

export function addressFixture(participant_id: number, type: string, value: string, fields: Partial<ParticipantAddress> = {}): ParticipantAddress {
  const address = { participant_id, type, value, display_value: '', service_slug: type, service_label: ['email', 'phone'].includes(type) ? '' : type, scope_kind: '', scope_value: '', participant_display_name: '', ...fields };
  return { identity_key: `fixture-${participant_id}-${type}-${value}`, ...address };
}
