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
    ]);
    expect(groups.map(group => [group.label, group.children?.map(child => child.label)])).toEqual([
      ['Family', ['Spouse', 'Guardian', 'Dependent']], ['Work', ['Colleague', 'Co-worker']], ['Social', ['Buddy', 'Acquaintance', 'Met']],
      ['Other', ['Mentors', 'Learns from']],
    ]);
    expect(relationshipTypeOptions([type('friend', 'friend')]).map(group => group.label)).toEqual(['Social']);
  });

  it('uses canonical labels for mirrored and renamed inverse aliases', () => {
    const parent = type('parent', 'guardian', 'dependent', { id: 1, inverse_type_id: 2 });
    const child = type('child', 'dependent', 'guardian', { id: 2, inverse_type_id: 1, is_canonical: false });
    for (const alias of [child, { ...child, forward_label: 'descendant', reverse_label: 'parent' }]) {
      expect(relationshipTypeOptions([parent, alias])[0].children?.map(choice => [choice.name, choice.displayLabel])).toEqual([
        ['parent:outgoing', 'guardian'], ['parent:incoming', 'dependent'],
      ]);
    }
  });

  it('uses current label text for connecting words, including custom labels', () => {
    expect(relationshipPhrase('parent', 'outgoing', 'parent')).toBe('is a parent of');
    expect(relationshipPhrase('met', 'outgoing', 'met')).toBe('has met');
    expect(relationshipPhrase('custom', 'outgoing', 'mentors')).toBe('mentors');
  });
});

it('uses type and direction for phrasing when a custom label matches a built-in label', () => {
  const choices = relationshipTypeOptions([type('custom', 'parent', 'child', { ownership: 'user' })])[0].children!;
  expect(choices.map(choice => choice.displayLabel)).toEqual(['parent', 'child']);
});

it.each([
  ['friend', 'outgoing', 'buddy', 'buddy'],
  ['parent', 'incoming', 'dependent', 'dependent'],
  ['agent', 'incoming', 'representative', 'representative'],
  ['agent', 'outgoing', 'pal', 'pal'],
] as const)('keeps the catalog label for renamed %s %s', (slug, direction, label, expected) => {
  expect(relationshipPhrase(slug, direction, label)).toBe(expected);
});

it('orders custom directions consistently regardless of input order and fills empty reverse labels', () => {
  const a = type('zeta', 'zeta', 'alpha');
  const b = type('b', 'beta', 'gamma');
  const names = (types: RelationshipType[]) => relationshipTypeOptions(types)[0].children!.map(choice => choice.name);
  expect(names([a, b])).toEqual(names([b, a]));
  const choices = relationshipTypeOptions([type('custom', 'mentor', '')])[0].children!;
  expect(choices.map(choice => choice.label)).toEqual(['Mentor', 'Mentor']);
});
