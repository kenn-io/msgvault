import type { ExploreGroupOverview } from './models';

export function bucketSeries(activity: ExploreGroupOverview['activity']): ExploreGroupOverview['activity'] {
  if (!activity.length) return [];
  const sorted = [...activity].sort((a, b) => a.bucket.localeCompare(b.bucket));
  const counts = new Map(sorted.map(item => [item.bucket, item.count]));
  const first = sorted[0]!.bucket, last = sorted.at(-1)!.bucket;
  const daily = first.length === 10;
  const date = new Date(`${first}${daily ? '' : '-01'}T00:00:00Z`);
  const result = [];
  for (;;) {
    const bucket = date.toISOString().slice(0, daily ? 10 : 7);
    if (bucket > last) break;
    result.push({ bucket, count: counts.get(bucket) ?? 0 });
    if (daily) date.setUTCDate(date.getUTCDate() + 1);
    else date.setUTCMonth(date.getUTCMonth() + 1);
  }
  return result;
}
