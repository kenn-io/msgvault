/** Archive evidence by how directly it speaks about the person. */
const ARCHIVE_SOURCE: Record<string, string> = {
  'direct-self': 'From a message they sent',
  'direct-other': 'From a message about them',
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

/** Evidence authority; ordinary says nothing. */
const AUTHORITY_WORDS: Record<'authoritative' | 'ordinary' | 'aggregator', string | null> = {
  authoritative: 'official record',
  ordinary: null,
  aggregator: 'aggregator',
};
export function authorityWords(authority: string): string | null {
  return AUTHORITY_WORDS[authority as keyof typeof AUTHORITY_WORDS] ?? null;
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
  switch (outcome) {
    case 'saved': return value ? `Saved: ${value}` : 'Saved';
    case 'value-retired': return 'This value is no longer saved';
    case 'current-value-kept': return reason === 'insufficient-margin' ? `Kept the current value; ${value ?? 'the claim'} wasn't far enough ahead` : 'Kept the current value';
    case 'pinned-value-kept': return 'Kept your pinned value';
    case 'too-close-to-choose': return 'The evidence was too close to choose';
    case 'not-used': return NOT_USED_REASONS[reason] ? `Not used: ${NOT_USED_REASONS[reason]}` : 'Not used';
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

/**
 * Why a source's standing changed. A lost source is its own status ("Source deleted");
 * a source that came back is the row's change, capitalized ("Imported again").
 */
const STATUS_REASON_WORDS: Record<string, string> = {
  'source-deleted': 'Source deleted',
  'source-edited': 'Source edited',
  'scope-unlinked': 'Unlinked from this person',
  'identity-reassigned': 'Moved to another person',
  'source-reimported': 'imported again',
  'scope-relinked': 'linked again',
};
function statusReasonWords(reason: string): string {
  return STATUS_REASON_WORDS[reason] ?? 'changed';
}
/**
 * One source history row's words before its date: the change, "Imported again" or
 * "Source deleted". Every source starts in the archive, so "In the archive" shows
 * only when a row names no change.
 */
export function statusEventWords(supported: boolean, reason: string): string {
  if (!supported) return lostSupportWords(reason);
  const words = statusReasonWords(reason);
  return ['source-reimported', 'scope-relinked'].includes(reason) ? words[0]!.toUpperCase() + words.slice(1) : 'In the archive';
}
/** The one exception an evidence row shows when it lost its source: "Source deleted". */
export function lostSupportWords(reason: string | undefined): string {
  const words = reason && !['source-reimported', 'scope-relinked'].includes(reason) ? statusReasonWords(reason) : null;
  return words && words !== 'changed' ? words : 'Source removed';
}

function strengthWords(score: number): string {
  return score >= 800 ? 'Strong' : score >= 600 ? 'Moderate' : 'Weak';
}
/** A recorded confidence or identity score: "Strong · 870 of 1000". */
export function scoreWords(score: number): string {
  return `${strengthWords(score)} · ${score} of 1000`;
}
