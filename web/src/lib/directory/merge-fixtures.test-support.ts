import type { ParticipantAddress } from '../api/generated/models';

export function identityKeyFixture(service: string, normalized: string, scopeKind = '', scopeValue = ''): string {
  return JSON.stringify([service, scopeKind, scopeValue, normalized]);
}

export function addressFixture(participant_id: number, type: string, value: string, fields: Partial<ParticipantAddress> = {}): ParticipantAddress {
  const address = { participant_id, type, value, display_value: '', service_slug: type, service_label: ['email', 'phone'].includes(type) ? '' : type, scope_kind: '', scope_value: '', participant_display_name: '', ...fields };
  return { identity_key: identityKeyFixture(address.service_slug, value, address.scope_kind, address.scope_value), ...address };
}
