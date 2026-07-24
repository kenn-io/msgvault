import { describe, expect, it } from 'vitest';

import {
  GROUPING_CATALOG,
  groupingByDimension,
  groupingOptions,
  isGroupingDimension,
  validateGroupingChain
} from './catalog';

describe('universal grouping catalog', () => {
  it('is the ordered source of truth for every canonical analytical group', () => {
    expect(GROUPING_CATALOG.map((entry) => entry.concept)).toEqual([
      'people',
      'identifiers',
      'domains',
      'time',
      'source',
      'modality',
      'labels',
      'attachment_facts',
      'conversation'
    ]);
    expect(groupingOptions().map((entry) => entry.value)).toEqual(
      ['participant', 'domain', 'year', 'month', 'source', 'message_type']
    );
    expect(groupingByDimension('participant')).toMatchObject({
      label: 'People',
      family: 'people'
    });
    expect(groupingByDimension('year')).toMatchObject({ family: 'time' });
  });

  it('keeps unsupported concepts visible but incapable of forming API requests', () => {
    for (const concept of ['identifiers', 'labels', 'attachment_facts', 'conversation'] as const) {
      const entry = GROUPING_CATALOG.find((candidate) => candidate.concept === concept);
      expect(entry).toMatchObject({ requestable: false });
      expect(entry?.unavailableReason).toBeTruthy();
      expect(entry?.requestDimensions).toEqual([]);
    }
    expect(groupingOptions({ includeUnavailable: true })).toEqual(expect.arrayContaining([
      expect.objectContaining({
        value: 'unavailable:labels',
        label: expect.stringContaining('Label grouping is not in the analytical API yet.'),
        disabled: true
      }),
      expect.objectContaining({ value: 'unavailable:attachment_facts', disabled: true })
    ]));
    expect(isGroupingDimension('kind')).toBe(false);
  });

  it('validates URL and Saved View grouping without accepting unknown dimensions', () => {
    expect(isGroupingDimension('source')).toBe(true);
    expect(isGroupingDimension('labels')).toBe(false);
    expect(validateGroupingChain(['participant', 'year'])).toEqual(['participant', 'year']);
    expect(validateGroupingChain(['domain', 'message_type'])).toEqual(['domain', 'message_type']);
    expect(validateGroupingChain(['source', 'month'])).toEqual(['source', 'month']);
    expect(validateGroupingChain(['source', 'kind'])).toEqual([]);
  });
});
