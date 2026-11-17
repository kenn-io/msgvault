/** Archive evidence by how directly it speaks about the person. */
const ARCHIVE_SOURCE: Record<string, string> = {
  'direct-self': 'From a message they sent',
  'direct-other': 'From a message or meeting',
  indirect: 'Inferred from your messages',
};
const OTHER_SOURCE: Record<string, string> = {
  public: 'From a public page',
  provider_assertion: 'From a lookup',
  system: 'Recorded automatically',
};

/** Where a piece of evidence came from, in words: "From a message they sent". */
export function evidenceSourceWords(sourceClass: string, directness: string): string {
  return (sourceClass === 'archive' ? ARCHIVE_SOURCE[directness] : OTHER_SOURCE[sourceClass]) ?? '';
}

/** Who proposed a claim. */
const ORIGIN_WORDS: Record<'extraction' | 'enrichment' | 'brief' | 'system' | 'invalid', string> = {
  extraction: 'From a message',
  enrichment: 'From a lookup',
  brief: 'From a brief',
  system: 'msgvault',
  invalid: 'Unreadable submission',
};
export function originWords(origin: string): string {
  return ORIGIN_WORDS[origin as keyof typeof ORIGIN_WORDS] ?? '';
}

/** A claim's relation; a supporting claim is the normal case and says nothing. */
const RELATION_WORDS: Record<'support' | 'contradict' | 'supersede' | 'invalid', string | null> = {
  support: null,
  contradict: 'Says this value isn’t true',
  supersede: 'Reports a replacement',
  invalid: 'Not usable',
};
export function relationWords(relation: string): string | null {
  return relation in RELATION_WORDS ? RELATION_WORDS[relation as keyof typeof RELATION_WORDS] : null;
}

const NOT_USED_REASONS: Record<string, string> = {
  'malformed-value': "the value couldn't be read", 'unsupported-target': "the field can't hold it",
  'stale-target-revision': "the field definition doesn't match the claim", 'unaligned-evidence': "the source doesn't mention it",
  'identity-mismatch': 'may be about someone else', 'sensitive-policy': 'this field is sensitive',
  'pin-retained': 'this field is pinned', 'below-threshold': 'score below threshold',
  'competing-tie': 'tied with another value',
  'explicit-contradiction': 'a source contradicts it', 'explicit-supersession': 'a newer statement replaced it',
  'organization-ambiguous': 'the organization is unclear', 'applied-projection': 'another claim was saved',
  'evidence-unsupported': 'its source was removed', 'outside-validity': 'outside its valid dates',
};
export function decisionSentence(outcome: string | null, reason: string, value?: string): string {
  if (!outcome || !value) return 'Result unavailable';
  switch (outcome) {
    case 'saved': return `Saved: ${value}`;
    case 'value-retired': return `This value is no longer saved: ${value}`;
    case 'current-value-kept':
      if (reason === 'insufficient-margin') return `Kept the current value; ${value} wasn't far enough ahead`;
      return `${value}: Kept the current value`;
    case 'pinned-value-kept': return `${value}: Kept your pinned value`;
    case 'too-close-to-choose': return `The evidence was too close to choose: ${value}`;
    case 'not-used': {
      const reasonWords = NOT_USED_REASONS[reason];
      return reasonWords ? `Not used: ${value} · ${reasonWords}` : `Not used: ${value}`;
    }
    default: return 'Result unavailable';
  }
}
export const SCORE_PARTS = [
  ['sourceClass', 'Source'], ['directness', 'Directness'], ['authority', 'Authority'],
  ['corroboration', 'Corroboration'], ['freshness', 'Freshness'], ['confidence', 'Confidence'],
] as const;

/** Where a saved decision put its value: a person's field, or their work. */
export function projectionWords(kind: string | null): string | null {
  if (kind === 'employment') return 'Work';
  return null;
}

/** A recorded confidence or identity score. */
export function scoreWords(score: number): string {
  return `${score} of 1000`;
}
