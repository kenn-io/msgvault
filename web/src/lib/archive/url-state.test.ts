import { describe, expect, it } from 'vitest';

import type { ArchiveURLState } from './types';
import {
  defaultArchiveURLState,
  parseArchiveURLState,
  serializeArchiveURLState
} from './url-state';

describe('archive URL state', () => {
  it('round-trips the complete restorable browse context', () => {
    const state: ArchiveURLState = {
      schemaVersion: 1,
      query: 'from:alice quarterly plan',
      searchMode: 'hybrid',
      filters: [
        { field: 'message_type', operator: 'equals', value: 'email' },
        { field: 'sent_at', operator: 'after', value: '2025-01-01' }
      ],
      groupingChain: ['person', 'year'],
      presentation: 'table',
      sort: [{ field: 'sent_at', direction: 'desc' }],
      columns: ['kind', 'people', 'subject', 'time'],
      selectedRow: 'message:42',
      inspectorPinned: true,
      conversationAnchor: 'message:37',
      scrollKey: 'message:31'
    };

    expect(parseArchiveURLState(serializeArchiveURLState(state))).toEqual(state);
  });

  it('preserves unknown future fields inside the versioned state envelope', () => {
    const futureState = {
      ...defaultArchiveURLState,
      schemaVersion: 2,
      futurePresentationOption: { density: 'compact' }
    } as ArchiveURLState;

    expect(parseArchiveURLState(serializeArchiveURLState(futureState))).toMatchObject({
      schemaVersion: 2,
      futurePresentationOption: { density: 'compact' }
    });
  });

  it('explicitly drops bulk selection from browser-restorable state', () => {
    const stateWithSelection = {
      ...defaultArchiveURLState,
      selectedRow: 'message:42',
      bulkSelection: ['message:42', 'message:43']
    } as unknown as ArchiveURLState;

    const restored = parseArchiveURLState(serializeArchiveURLState(stateWithSelection));

    expect(restored.selectedRow).toBe('message:42');
    expect(restored).not.toHaveProperty('bulkSelection');
  });

  it('uses defaults for a missing or malformed state parameter', () => {
    expect(parseArchiveURLState('')).toEqual(defaultArchiveURLState);
    expect(parseArchiveURLState('?state=%7Bnot-json')).toEqual(defaultArchiveURLState);
  });

  it('returns independent defaults for missing and malformed state', () => {
    const missing = parseArchiveURLState('');
    const malformed = parseArchiveURLState('?state=%7Bnot-json');

    missing.filters.push({ field: 'message_type', operator: 'equals', value: 'email' });
    missing.groupingChain.push('person');
    missing.sort.push({ field: 'sent_at', direction: 'desc' });
    missing.columns.push('attachment_count');

    expect(malformed).toEqual(defaultArchiveURLState);
    expect(missing.filters).not.toBe(defaultArchiveURLState.filters);
    expect(missing.groupingChain).not.toBe(defaultArchiveURLState.groupingChain);
    expect(missing.sort).not.toBe(defaultArchiveURLState.sort);
    expect(missing.columns).not.toBe(defaultArchiveURLState.columns);
  });
});
