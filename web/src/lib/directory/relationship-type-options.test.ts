import { relationshipType } from './entity-fixtures.test-support';
import { describe, expect, it } from 'vitest';
import type { RelationshipType } from './models';
import { relationshipPhrase, relationshipTypeOptions } from './relationship-type-options';

const type = (slug: string, forward: string, reverse = forward, overrides = {}): RelationshipType => relationshipType({
  slug, forward_label: forward, reverse_label: reverse, is_symmetric: forward === reverse, ownership: 'system', ...overrides,
});

describe('relationship choices', () => {
  it('orders groups and types by use, keeps live labels and hyphens, and omits empty groups', () => {
    const groups = relationshipTypeOptions([
      type('met', 'met'), type('acquaintance', 'acquaintance'), type('friend', 'buddy'), type('co-worker', 'co-worker'), type('colleague', 'colleague'),
      type('parent', 'guardian', 'dependent'), type('spouse', 'spouse'), type('custom', 'mentors', 'learns from', { ownership: 'user' }),
      type('a', 'sponsor', 'sponsor', { ownership: 'user' }), type('z', 'advisor', 'advisor', { ownership: 'user' }),
    ]);
    expect(groups.map(group => [group.label, group.children?.map(child => child.label)])).toEqual([
      ['Family', ['Spouse', 'Guardian', 'Dependent']], ['Work', ['Colleague', 'Co-worker']], ['Social', ['Buddy', 'Acquaintance', 'Met']],
      ['Other', ['Advisor', 'Learns from', 'Mentors', 'Sponsor']],
    ]);
    expect(relationshipTypeOptions([type('friend', 'friend')]).map(group => group.label)).toEqual(['Social']);
  });

  it('uses canonical labels for mirrored and renamed inverse aliases', () => {
    const parent = type('parent', 'guardian', 'dependent', { id: 1, inverse_type_id: 2 });
    const child = type('child', 'dependent', 'guardian', { id: 2, inverse_type_id: 1, is_canonical: false });
    for (const alias of [child]) {
      expect(relationshipTypeOptions([parent, alias])[0].children?.map(choice => [choice.name, choice.displayLabel])).toEqual([
        ['parent:outgoing', 'guardian'], ['parent:incoming', 'dependent'],
      ]);
    }
  });

  it('uses current label text for connecting words, including custom labels', () => {
    expect(relationshipPhrase('parent', 'outgoing', 'parent')).toBe('is a parent of');
    expect(relationshipPhrase('met', 'outgoing', 'met')).toBe('has met');
    expect(relationshipPhrase('custom', 'outgoing', 'mentors')).toBe('mentors');
    expect(relationshipPhrase('custom', 'outgoing', 'parent')).toBe('parent');
  });
});
