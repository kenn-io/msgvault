import { relationshipType } from './entity-fixtures.test-support';
import { describe, expect, it } from 'vitest';
import type { RelationshipType } from './models';
import { relationshipPhrase, relationshipTypeOptions } from './relationship-type-options';

const type = (slug: string, forward: string, reverse = forward, overrides = {}): RelationshipType => relationshipType({
  slug, forward_label: forward, reverse_label: reverse, is_symmetric: forward === reverse, ...overrides,
});

describe('relationship choices', () => {
  it('groups system slugs, sorts live labels, preserves hyphens, and omits empty groups', () => {
    const groups = relationshipTypeOptions([
      type('friend', 'buddy'), type('met', 'met'), type('co-worker', 'co-worker'), type('parent', 'guardian', 'dependent'),
      type('custom', 'mentors', 'learns from', { ownership: 'user' }),
    ]);
    expect(groups.map(group => [group.label, group.children?.map(child => child.label)])).toEqual([
      ['Family', ['Dependent', 'Guardian']], ['Work', ['Co-worker']], ['Social', ['Buddy', 'Met']], ['Other', ['Learns from', 'Mentors']],
    ]);
    expect(relationshipTypeOptions([type('friend', 'friend')]).map(group => group.label)).toEqual(['Social']);
  });

  it('uses canonical labels for mirrored and renamed inverse aliases', () => {
    const parent = type('parent', 'guardian', 'dependent', { id: 1, inverse_type_id: 2 });
    const child = type('child', 'dependent', 'guardian', { id: 2, inverse_type_id: 1, is_canonical: false });
    expect(relationshipTypeOptions([parent, child])[0].children?.map(choice => choice.name)).toEqual(['parent:incoming', 'parent:outgoing']);
    expect(relationshipTypeOptions([parent, { ...child, forward_label: 'descendant', reverse_label: 'parent' }])[0].children?.map(choice => [choice.name, choice.displayLabel])).toEqual([
      ['parent:incoming', '→ dependent →'], ['parent:outgoing', '→ guardian →'],
    ]);
  });

  it('distinguishes captions shared across types in menus and selections', () => {
    const choices = relationshipTypeOptions([type('manages', 'manages', 'reports to'), type('supervises', 'supervises', 'reports to')])[0].children!;
    expect(choices.filter(choice => choice.name.endsWith(':incoming')).map(choice => [choice.label, choice.displayLabel])).toEqual([
      ['Reports to (manages; person → subject)', '→ reports to → (manages; person → subject)'],
      ['Reports to (supervises; person → subject)', '→ reports to → (supervises; person → subject)'],
    ]);
  });

  it('uses current label text for connecting words, including custom labels', () => {
    expect(relationshipPhrase('parent')).toBe('is a parent of');
    expect(relationshipPhrase('met')).toBe('has met');
    expect(relationshipPhrase('mentors')).toBe('→ mentors →');
    expect(relationshipPhrase('guardian')).toBe('→ guardian →');
  });
});
