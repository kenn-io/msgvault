const MINUTE_MS = 60_000;
const HOUR_MS = 60 * MINUTE_MS;
const DAY_MS = 24 * HOUR_MS;

function plural(count: number, unit: string): string {
  return `${count} ${unit}${count === 1 ? '' : 's'} ago`;
}

/** "3 days ago", "2 months ago". Unparseable input passes through so raw API values stay visible. */
export function relativeDate(value: string | null | undefined, now: Date = new Date()): string {
  if (!value) return '';
  const date = new Date(value);
  if (Number.isNaN(date.valueOf())) return value;
  const elapsed = now.getTime() - date.getTime();
  if (elapsed < HOUR_MS) return elapsed < MINUTE_MS ? 'just now' : plural(Math.floor(elapsed / MINUTE_MS), 'minute');
  if (elapsed < DAY_MS) return plural(Math.floor(elapsed / HOUR_MS), 'hour');
  const days = Math.floor(elapsed / DAY_MS);
  if (days < 14) return days === 1 ? 'yesterday' : plural(days, 'day');
  if (days < 60) return plural(Math.floor(days / 7), 'week');
  if (days < 730) return plural(Math.max(2, Math.floor(days / 30.4)), 'month');
  return plural(Math.floor(days / 365.25), 'year');
}

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
