import type { TypeaheadOption } from '@kenn-io/kit-ui';
import type { RelationshipType } from './models';
import relationshipPhrases from './relationship-phrases.json';

type Orientation = 'outgoing' | 'incoming';
export type RelationshipChoice = TypeaheadOption & { slug: string; direction: Orientation; group: string };

// Groups in order of use, and each group's types with the common ones first; types not listed fall in Other, by label.
// Each phrase reads a seeded label as a sentence.
const { groups, phrases }: { groups: Record<string, string[]>; phrases: Record<string, string[]> } = relationshipPhrases;
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
