import type { SelectDropdownOption } from '@kenn-io/kit-ui';

import type { ExploreGroupDimension } from '../explore/models';

export type GroupingConcept =
  | 'people'
  | 'identifiers'
  | 'domains'
  | 'time'
  | 'source'
  | 'modality'
  | 'labels'
  | 'attachment_facts'
  | 'conversation';

export type GroupingFamily = GroupingConcept;

export interface GroupingCatalogEntry {
  concept: GroupingConcept;
  label: string;
  family: GroupingFamily;
  keywords: string;
  requestable: boolean;
  requestDimensions: readonly ExploreGroupDimension[];
  unavailableReason?: string;
}

/**
 * One product-level grouping catalog for controls, commands, URL validation,
 * and Saved Views. Unsupported concepts stay visible and explicit, but have no
 * request dimension and therefore can never leak an invalid API value.
 */
export const GROUPING_CATALOG: readonly GroupingCatalogEntry[] = [
  {
    concept: 'people',
    label: 'People',
    family: 'people',
    keywords: 'person sender recipient email phone identity',
    requestable: true,
    requestDimensions: ['participant']
  },
  {
    concept: 'identifiers',
    label: 'Identifiers',
    family: 'identifiers',
    keywords: 'email phone address account identity',
    requestable: false,
    requestDimensions: [],
    unavailableReason: 'Identifier-level grouping is not in the analytical API yet.'
  },
  {
    concept: 'domains',
    label: 'Domains',
    family: 'domains',
    keywords: 'organization host email domain',
    requestable: true,
    requestDimensions: ['domain']
  },
  {
    concept: 'time',
    label: 'Time',
    family: 'time',
    keywords: 'year month date annual monthly',
    requestable: true,
    requestDimensions: ['year', 'month']
  },
  {
    concept: 'source',
    label: 'Source',
    family: 'source',
    keywords: 'account archive provider',
    requestable: true,
    requestDimensions: ['source']
  },
  {
    concept: 'modality',
    label: 'Modality',
    family: 'modality',
    keywords: 'type email chat text calendar meeting note',
    requestable: true,
    requestDimensions: ['message_type']
  },
  {
    concept: 'labels',
    label: 'Labels',
    family: 'labels',
    keywords: 'tag category mailbox',
    requestable: false,
    requestDimensions: [],
    unavailableReason: 'Label grouping is not in the analytical API yet.'
  },
  {
    concept: 'attachment_facts',
    label: 'Attachment facts',
    family: 'attachment_facts',
    keywords: 'file attachment size type extension',
    requestable: false,
    requestDimensions: [],
    unavailableReason: 'Attachment-fact grouping is reserved for the Files workspace.'
  },
  {
    concept: 'conversation',
    label: 'Conversation',
    family: 'conversation',
    keywords: 'thread chat logical entry',
    requestable: false,
    requestDimensions: [],
    unavailableReason: 'Conversation grouping is not filterable in the analytical API yet.'
  }
] as const;

const requestableDimensions = new Set<ExploreGroupDimension>(
  GROUPING_CATALOG.flatMap((entry) => entry.requestDimensions)
);

export function isGroupingDimension(value: unknown): value is ExploreGroupDimension {
  return typeof value === 'string' && requestableDimensions.has(value as ExploreGroupDimension);
}

export function validateGroupingChain(value: unknown): ExploreGroupDimension[] {
  return Array.isArray(value) && value.every(isGroupingDimension)
    ? [...value]
    : [];
}

export function groupingByDimension(
  dimension: ExploreGroupDimension
): GroupingCatalogEntry {
  return GROUPING_CATALOG.find((entry) => entry.requestDimensions.includes(dimension))!;
}

export function groupingDimensionLabel(dimension: ExploreGroupDimension): string {
  const entry = groupingByDimension(dimension);
  if (entry.concept !== 'time') return entry.label;
  return dimension === 'year' ? 'Year' : 'Month';
}

export function groupingOptions(
  options: {
    excluded?: readonly ExploreGroupDimension[];
    includeUnavailable?: boolean;
  } = {}
): SelectDropdownOption[] {
  const excluded = new Set(options.excluded ?? []);
  const result: SelectDropdownOption[] = [];
  for (const entry of GROUPING_CATALOG) {
    for (const dimension of entry.requestDimensions) {
      if (!excluded.has(dimension)) {
        result.push({ value: dimension, label: groupingDimensionLabel(dimension) });
      }
    }
    if (options.includeUnavailable && !entry.requestable) {
      result.push({
        value: `unavailable:${entry.concept}`,
        label: `${entry.label} — unavailable: ${entry.unavailableReason}`,
        disabled: true
      });
    }
  }
  return result;
}
