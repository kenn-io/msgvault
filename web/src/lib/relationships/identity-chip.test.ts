import { describe, expect, it } from 'vitest';

import type { PersonClusterEdge, PersonClusterLinkOrigin, PersonClusterMember, PersonIdentifier } from '../api/generated/models';
import { identityChipText } from './identity-chip';

const identifier = (overrides: Partial<PersonIdentifier> = {}): PersonIdentifier => ({
  type: 'beeper', value: 'beeper:8:whatsapp:9:@user:x.y',
  display_value: 'beeper:8:whatsapp:9:@user:x.y', participant_id: 10,
  is_primary: true, provenance: 'participant_identifiers', ...overrides
});

const edge = (participantB: number, linkOrigin: PersonClusterLinkOrigin): PersonClusterEdge => ({
  participant_a: 1, participant_b: participantB, link_origin: linkOrigin
});

describe('identityChipText', () => {
  it('uses service, member and scope context without showing an opaque key', () => {
    const text = identityChipText(identifier({
      service_label: 'WhatsApp', participant_display_name: 'Alias Example',
      scope_kind: 'account', scope_value: 'local-whatsapp_ba_example'
    }), undefined, [edge(10, { kind: 'manual' })]);

    expect(text.title).toBe('WhatsApp');
    expect(text.subtitle).toContain('Alias Example');
    expect(text.subtitle).not.toContain('local-whatsapp_ba_example');
    expect(text.detail).toContain('account: local-whatsapp_ba_example');
    expect(text.subtitle).toContain('linked manually');
    expect(`${text.title} ${text.subtitle}`).not.toContain(identifier().value);
    expect(text.detail).toContain(identifier().value);
  });

  it('uses the provider label when no service classification is stored', () => {
    const text = identityChipText(identifier({ service_label: undefined }), undefined, []);
    expect(text.title).toBe('Beeper');
  });

  it.each([
    ['archive_observation', 'stable_provider_id', 'matched from archived contacts (same provider identity)'],
    ['archive_observation', 'service_scope_username', 'matched from archived contacts (same username on a service)'],
    ['user', 'display_name', 'matched from user input (same display name)']
  ])('explains %s / %s', (source, basis, expected) => {
    const text = identityChipText(identifier(), undefined, [edge(10, { kind: 'candidate', source, basis })]);
    expect(text.subtitle).toBe(expected);
  });

  it('keeps a server scope visible', () => {
    const text = identityChipText(identifier({ scope_kind: 'server', scope_value: 'matrix.example.com' }), undefined, []);
    expect(text.subtitle).toBe('server: matrix.example.com');
  });

  it('keeps distinct ordinary email and phone addresses on their chip faces', () => {
    const first = identityChipText(identifier({ type: 'email', value: 'first@example.com', display_value: '' }), undefined, []);
    const second = identityChipText(identifier({ type: 'email', value: 'second@example.com', display_value: '' }), undefined, []);
    const phone = identityChipText(identifier({ type: 'phone', value: '+15550100001', display_value: '' }), undefined, []);

    expect(`${first.title} ${first.subtitle}`).toContain('first@example.com');
    expect(`${second.title} ${second.subtitle}`).toContain('second@example.com');
    expect(`${phone.title} ${phone.subtitle}`).toContain('+15550100001');
  });

  it('uses a member address when there is no identifier row', () => {
    const member = { participant_id: 10, email: 'bare@example.com' } as PersonClusterMember;
    const text = identityChipText(undefined, member, [edge(10, { kind: 'candidate', source: 'carddav_import', basis: 'email' })]);

    expect(`${text.title} ${text.subtitle}`).toContain('bare@example.com');
    expect(text.subtitle).toContain('matched from CardDAV (email)');
  });

  it('shows all distinct incident origins in a stable order', () => {
    const edges = [
      edge(10, { kind: 'candidate', source: 'carddav_import', basis: 'email' }),
      { participant_a: 10, participant_b: 12, link_origin: { kind: 'manual' } }
    ] as PersonClusterEdge[];
    const text = identityChipText(identifier(), undefined, edges);

    expect(text.subtitle).toContain('linked manually');
    expect(text.subtitle).toContain('matched from CardDAV (email)');
    expect(text.subtitle.indexOf('linked manually')).toBeLessThan(text.subtitle.indexOf('matched from CardDAV'));
  });

  it('says no stored address only when the member has no name, email, or phone', () => {
    const unknown = identityChipText(undefined, { participant_id: 10 } as PersonClusterMember, []);
    const named = identityChipText(undefined, { participant_id: 10, display_name: 'Bare Example' } as PersonClusterMember, []);

    expect(unknown.subtitle).toContain('no stored address');
    expect(`${named.title} ${named.subtitle}`).toContain('Bare Example');
    expect(named.subtitle).not.toContain('no stored address');
  });
});
