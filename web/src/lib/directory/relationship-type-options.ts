import type { TypeaheadOption } from '@kenn-io/kit-ui';
import type { RelationshipType } from './models';

type Orientation = 'outgoing' | 'incoming';
export type RelationshipChoice = TypeaheadOption & { slug: string; direction: Orientation; group: string };

// Groups in order of use, and each group's types with the common ones first. Each type appears once;
// types not listed fall in Other, by label.
const groups: Record<string, string[]> = {
  Family: ['spouse', 'parent', 'sibling', 'kin'],
  Work: ['colleague', 'co-worker', 'agent'],
  Social: ['friend', 'date', 'sweetheart', 'acquaintance', 'neighbor', 'met', 'co-resident', 'crush', 'muse'],
  Other: ['contact'],
};
const phrases: Record<string, [string, string]> = {
  'parent:outgoing': ['parent', 'is a parent of'], 'parent:incoming': ['child', 'is a child of'],
  'sibling:outgoing': ['sibling', 'is a sibling of'], 'spouse:outgoing': ['spouse', 'is a spouse of'],
  'kin:outgoing': ['relative', 'is a relative of'],
  'agent:outgoing': ['agent', 'is an agent for'], 'agent:incoming': ['principal', 'is represented by'],
  'colleague:outgoing': ['colleague', 'is a colleague of'], 'co-worker:outgoing': ['co-worker', 'is a co-worker of'],
  'acquaintance:outgoing': ['acquaintance', 'is an acquaintance of'], 'friend:outgoing': ['friend', 'is a friend of'],
  'met:outgoing': ['met', 'has met'], 'neighbor:outgoing': ['neighbor', 'is a neighbor of'],
  'co-resident:outgoing': ['co-resident', 'is a co-resident of'], 'date:outgoing': ['date', 'is dating'],
  'sweetheart:outgoing': ['sweetheart', 'is a sweetheart of'], 'crush:outgoing': ['crush', 'is a crush of'],
  'crush:incoming': ['admirer', 'has a crush on'], 'muse:outgoing': ['muse', 'is a muse to'],
  'muse:incoming': ['inspired by', 'is inspired by'], 'contact:outgoing': ['contact', 'is a contact of'],
  'emergency:outgoing': ['emergency contact', 'is an emergency contact for'], 'emergency:incoming': ['emergency contact for', 'has emergency contact'],
};
export function relationshipPhrase(slug: string, direction: Orientation, label: string): string {
  const phrase = phrases[`${slug}:${direction}`];
  return phrase && label === phrase[0] ? phrase[1] : label;
}

function rank(group: string, choice: RelationshipChoice): number {
  const index = groups[group].indexOf(choice.slug);
  return index < 0 ? groups[group].length : index;
}

export function relationshipTypeOptions(types: RelationshipType[]): TypeaheadOption[] {
  const choices = types.flatMap((type): RelationshipChoice[] => {
    if (!type.is_canonical) return [];
    const group = Object.keys(groups).find(name => groups[name].includes(type.slug)) ?? 'Other';
    return (type.is_symmetric ? ['outgoing'] as const : ['outgoing', 'incoming'] as const).map(direction => {
      const label = direction === 'outgoing' ? type.forward_label : type.reverse_label;
      return { name: `${type.slug}:${direction}`, slug: type.slug, direction, group, label: label.charAt(0).toUpperCase() + label.slice(1), displayLabel: relationshipPhrase(type.slug, direction, label) };
    });
  });
  return Object.keys(groups).map(group => ({
    name: group, label: group,
    children: choices.filter(choice => choice.group === group).sort((a, b) => rank(group, a) - rank(group, b)
      || (rank(group, a) === groups[group].length ? a.label.localeCompare(b.label) : 0)
      || a.slug.localeCompare(b.slug) || a.direction.localeCompare(b.direction) * -1),
  })).filter(group => group.children.length > 0);
}
