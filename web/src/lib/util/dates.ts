/** Superhuman-style compact timestamp for list rows: recent activity reads
 * as an age ("5m", "3h", "2d"), older activity this year as a short date
 * ("Jun 29"), and anything before this year collapses to the year ("2024").
 * Unparseable input passes through untouched so raw API values stay visible
 * instead of turning into "Invalid Date". */
export function compactDate(value: string, now: Date = new Date()): string {
  const date = new Date(value);
  if (Number.isNaN(date.valueOf())) return value;

  const elapsedMs = now.getTime() - date.getTime();
  const minuteMs = 60_000;
  const hourMs = 60 * minuteMs;
  const dayMs = 24 * hourMs;

  if (elapsedMs >= 0 && elapsedMs < hourMs) {
    return `${Math.max(1, Math.floor(elapsedMs / minuteMs))}m`;
  }
  if (elapsedMs >= 0 && elapsedMs < dayMs) {
    return `${Math.floor(elapsedMs / hourMs)}h`;
  }
  if (elapsedMs >= 0 && elapsedMs < 7 * dayMs) {
    return `${Math.floor(elapsedMs / dayMs)}d`;
  }
  if (date.getFullYear() === now.getFullYear()) {
    return new Intl.DateTimeFormat('en-US', { month: 'short', day: 'numeric' }).format(date);
  }
  return String(date.getFullYear());
}
