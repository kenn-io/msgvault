export function messageCount(count: number): string {
  return `${count.toLocaleString()} ${count === 1 ? 'message' : 'messages'}`;
}

/** The names and values that identify a match, joined for one quiet line. */
export function joinParts(parts: Array<string | null | undefined | false>): string {
  return parts.filter(Boolean).join(' · ');
}

/** One line naming why two people look alike, such as "Same email". */
export function basisLine(candidate: {
  basis: string;
  normalized_value?: string;
  service_slug?: string;
}): string {
  const kind = candidate.basis.toLowerCase();
  let label: string;
  if (kind.includes('email')) label = 'Same email';
  else if (kind.includes('phone')) label = 'Same phone number';
  else if (kind.includes('username') || kind.includes('handle')) {
    label = candidate.service_slug ? `Same ${candidate.service_slug} username` : 'Same username';
  } else if (kind.includes('name')) label = 'Same name';
  else if (kind.includes('conversation')) label = 'In the same conversations';
  else label = 'Similar details';
  return candidate.normalized_value ? `${label}: ${candidate.normalized_value}` : label;
}

/** A readable reason a conflict can't apply as it stands. */
export function blockerLine(blocker: string | undefined): string {
  switch (blocker) {
    case 'person_merge_required':
      return 'Both are saved as different people, so merging needs a choice of which to keep.';
    case 'rejected':
      return 'Marked as different people before.';
    case undefined:
    case '':
      return 'This match needs a decision first.';
    default:
      return blocker.replaceAll('_', ' ').replace(/^./, (letter) => letter.toUpperCase()) + '.';
  }
}
