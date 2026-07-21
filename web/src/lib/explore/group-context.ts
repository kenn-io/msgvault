import { isGroupingDimension } from '../grouping/catalog';
import type { ExploreFilter, ExploreGroupDimension } from './models';

export interface GroupSelectionKey {
  dimension: ExploreGroupDimension;
  key: string;
}

export function parseGroupSelection(value: string | null | undefined): GroupSelectionKey | undefined {
  if (!value?.startsWith('group:')) return undefined;
  const separator = value.indexOf(':', 'group:'.length);
  if (separator < 0) return undefined;
  const dimension = value.slice('group:'.length, separator);
  const key = value.slice(separator + 1);
  return isGroupingDimension(dimension) && key ? { dimension, key } : undefined;
}

export function filtersForGroup(
  filters: readonly ExploreFilter[],
  dimension: ExploreGroupDimension,
  key: string
): ExploreFilter[] | undefined {
  if (
    dimension === 'source' ||
    dimension === 'participant' ||
    dimension === 'domain' ||
    dimension === 'message_type'
  ) {
    return [
      ...filters.filter((filter) => filter.dimension !== dimension),
      { dimension, values: [key] }
    ];
  }
  if (dimension === 'year' && /^\d{4}$/.test(key)) {
    const year = Number(key);
    return replaceTimeFilters(filters, `${key}-01-01T00:00:00Z`, `${year + 1}-01-01T00:00:00Z`);
  }
  if (dimension === 'month' && /^\d{4}-(0[1-9]|1[0-2])$/.test(key)) {
    const [year, month] = key.split('-').map(Number) as [number, number];
    const nextYear = month === 12 ? year + 1 : year;
    const nextMonth = month === 12 ? 1 : month + 1;
    return replaceTimeFilters(
      filters,
      `${key}-01T00:00:00Z`,
      `${nextYear}-${String(nextMonth).padStart(2, '0')}-01T00:00:00Z`
    );
  }
  return undefined;
}

function replaceTimeFilters(
  filters: readonly ExploreFilter[],
  after: string,
  before: string
): ExploreFilter[] | undefined {
  const existingAfter = filters.find((filter) => filter.dimension === 'after')?.values[0];
  const existingBefore = filters.find((filter) => filter.dimension === 'before')?.values[0];
  const lower = laterBound(after, existingAfter);
  const upper = earlierBound(before, existingBefore);
  if (Date.parse(lower) >= Date.parse(upper)) return undefined;
  return [
    ...filters.filter((filter) => filter.dimension !== 'after' && filter.dimension !== 'before'),
    { dimension: 'after', values: [lower] },
    { dimension: 'before', values: [upper] }
  ];
}

function laterBound(groupBound: string, existing: string | undefined): string {
  return existing && Number.isFinite(Date.parse(existing)) && Date.parse(existing) > Date.parse(groupBound)
    ? existing : groupBound;
}

function earlierBound(groupBound: string, existing: string | undefined): string {
  return existing && Number.isFinite(Date.parse(existing)) && Date.parse(existing) < Date.parse(groupBound)
    ? existing : groupBound;
}
