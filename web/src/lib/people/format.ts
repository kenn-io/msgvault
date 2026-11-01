import { identityBasisLabel } from '../relationships/identity-chip';

export function messageCount(count: number): string {
  return `${count.toLocaleString()} ${count === 1 ? 'message' : 'messages'}`;
}

/** The names and values that identify a match, joined for one quiet line. */
export function joinParts(parts: Array<string | null | undefined | false>): string {
  return parts.filter(Boolean).join(' · ');
}

/** The reason and matched value on a pair card. */
export function basisLine(candidate: {
  basis: string;
  normalized_value?: string;
}): string {
  const basis = identityBasisLabel(candidate.basis);
  const label = basis.replace(/^./, letter => letter.toUpperCase());
  return candidate.normalized_value ? `${label}: ${candidate.normalized_value}` : label;
}

/** A readable reason a conflict can't apply as it stands. */
export function blockerLine(blocker: string | undefined): string {
  switch (blocker) {
    case 'person_merge_required':
      return 'Both are saved as different people, so merging needs a choice of which to keep.';
    case undefined:
    case '':
      return 'This match needs a decision first.';
    default:
      return blocker.replaceAll('_', ' ').replace(/^./, (letter) => letter.toUpperCase()) + '.';
  }
}
