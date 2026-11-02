/**
 * UTC [start, end) bounds of the local calendar day containing `instant`,
 * per the host's local timezone (the same zone RelationshipsController
 * sends as `timezone()` — the day boundary the backend already bucketed the
 * burst on).
 */
export function localDayBoundsUTC(instant: string): { start: string; end: string } {
  const date = new Date(instant);
  const startOfDay = new Date(date.getFullYear(), date.getMonth(), date.getDate());
  // A skipped midnight can normalize the start to 01:00; do not carry that hour forward.
  const endOfDay = new Date(date.getFullYear(), date.getMonth(), date.getDate() + 1);
  return { start: startOfDay.toISOString(), end: endOfDay.toISOString() };
}
