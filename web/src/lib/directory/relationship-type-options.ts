import type { TypeaheadOption } from '@kenn-io/kit-ui';
import type { RelationshipType } from './models';

type Orientation = 'outgoing' | 'incoming';
export type RelationshipChoice = TypeaheadOption & { slug: string; direction: Orientation; group: string };

const groups: Record<string, string[]> = {
  Family: ['parent', 'sibling', 'spouse', 'kin', 'relative'],
  Work: ['agent', 'colleague', 'co-worker'],
  Social: ['acquaintance', 'friend', 'met', 'neighbor', 'co-resident', 'date', 'sweetheart', 'crush', 'muse'],
  Other: [],
};
const phrases: Record<string, string> = {
  parent: 'is a parent of', child: 'is a child of', sibling: 'is a sibling of', spouse: 'is a spouse of',
  relative: 'is a relative of', agent: 'is an agent for', principal: 'is represented by',
  colleague: 'is a colleague of', 'co-worker': 'is a co-worker of', acquaintance: 'is an acquaintance of',
  friend: 'is a friend of', met: 'has met', neighbor: 'is a neighbor of', 'co-resident': 'is a co-resident of',
  date: 'is dating', sweetheart: 'is a sweetheart of', crush: 'is a crush of', admirer: 'has a crush on',
  muse: 'is a muse to', 'inspired by': 'is inspired by', contact: 'is a contact of',
  'emergency contact': 'is an emergency contact for', 'emergency contact for': 'has emergency contact',
};
export function relationshipPhrase(label: string): string {
  return Object.hasOwn(phrases, label) ? phrases[label] : `→ ${label} →`;
}

export function relationshipTypeOptions(types: RelationshipType[]): TypeaheadOption[] {
  const choices = types.flatMap((type): RelationshipChoice[] => {
    if (!type.is_canonical) return [];
    const group = Object.keys(groups).find(name => groups[name].includes(type.slug)) ?? 'Other';
    return (type.is_symmetric ? ['outgoing'] as const : ['outgoing', 'incoming'] as const).map(direction => {
      const label = direction === 'outgoing' ? type.forward_label : type.reverse_label;
      return { name: `${type.slug}:${direction}`, slug: type.slug, direction, group, label: label.charAt(0).toUpperCase() + label.slice(1), displayLabel: relationshipPhrase(label) };
    });
  });
  const labels = choices.map(choice => choice.label);
  for (const choice of choices) {
    if (labels.filter(label => label === choice.label).length < 2) continue;
    const roles = choice.direction === 'outgoing' ? 'subject → person' : 'person → subject';
    const suffix = ` (${choice.slug}; ${roles})`;
    choice.label += suffix;
    choice.displayLabel += suffix;
  }
  return Object.keys(groups).map(group => ({
    name: group, label: group,
    children: choices.filter(choice => choice.group === group).sort((a, b) => a.label.localeCompare(b.label)),
  })).filter(group => group.children.length > 0);
}
