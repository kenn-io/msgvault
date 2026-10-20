import { describe, expect, it } from 'vitest';

import legacyPipes from '../../../../testdata/savedview_legacy_pipes.json';
import {
  defaultExploreURLState,
  ExploreState,
  parseExploreURLState,
  serializeExploreURLState
} from '../explore/state.svelte';
import type { ExploreURLState } from '../explore/models';
import { canonicalSavedViewState, exploreStateFromSavedView, savedViewSummary, type CanonicalState } from './canonical';

const everything: ExploreURLState = {
  ...defaultExploreURLState,
  workspace: 'everything',
  query: ' invoice ',
  searchMode: 'full_text',
  filters: [{ dimension: 'source', values: ['7'] }],
  groupingChain: ['year'],
  presentation: 'table',
  sort: [{ field: 'occurred_at', direction: 'desc' }],
  columns: ['kind', 'title'],
  activeRow: 'message:9',
  selectedRow: 'message:9'
};

function reopened(state: ExploreURLState): ExploreURLState {
  const opened = exploreStateFromSavedView(canonicalSavedViewState(state));
  return parseExploreURLState(serializeExploreURLState({ ...defaultExploreURLState, ...opened }));
}

function openedAndReloaded(saved: CanonicalState) {
  window.history.replaceState(null, '', '/');
  const state = new ExploreState(window);
  try {
    state.replaceCommittedNavigation(exploreStateFromSavedView(saved));
    return {
      opened: state.current.filters,
      reloaded: parseExploreURLState(window.location.search).filters
    };
  } finally {
    state.destroy();
  }
}

describe('saved-view canonical state', () => {
  it.each([
    [' 1', '1 ', 'SENDER'],
    ['01', '+1', 'any'],
    ['9223372036854775807', '+09223372036854775807', ' recipient '],
    ['\u00851', '1', 'any'],
    [['1', ' 1'], '1', 'sender']
  ])('keeps identity %j/%j/%j on open and URL-only reload', (source, identity, direction) => {
    const sources = [source].flat();
    const saved: CanonicalState = { filters: [
      { field: 'source', operator: 'in', values: sources },
      { field: 'identity', operator: 'in', values: [identity, 'person@example.com', direction] }
    ] };
    const expected = [
      { dimension: 'source', values: sources },
      { dimension: 'identity', values: [identity, 'person@example.com', direction] }
    ];
    expect(openedAndReloaded(saved)).toEqual({ opened: expected, reloaded: expected });
  });

  it.each([
    ['0', '0'], ['-1', '-1'], ['9223372036854775808', '9223372036854775808'],
    ['1', '2'], ['9007199254740992', '9007199254740993']
  ])('drops only identity for IDs %s and %s', (source, identity) => {
    const saved: CanonicalState = { filters: [
      { field: 'source', operator: 'in', values: [source] },
      { field: 'identity', operator: 'in', values: [identity, 'person@example.com', 'any'] },
      { field: 'message_type', operator: 'in', values: ['email'] }
    ] };
    const expected = [
      { dimension: 'source', values: [source] },
      { dimension: 'message_type', values: ['email'] }
    ];
    expect(openedAndReloaded(saved)).toEqual({ opened: expected, reloaded: expected });
  });

  it.each(legacyPipes)('opens schema $schema_version $search_mode query $query', ({ schema_version, search_mode, query, want }) => {
    expect(exploreStateFromSavedView({ query, search_mode: search_mode as ExploreURLState['searchMode'] | undefined }, schema_version).query).toBe(want);
  });

  it('round-trips an Everything view', () => {
    expect(canonicalSavedViewState(everything)).toEqual({
      query: 'invoice', search_mode: 'full_text',
      filters: [{ field: 'source', operator: 'in', values: ['7'] }],
      grouping: ['year'], presentation: 'table',
      sort: [{ field: 'occurred_at', direction: 'desc' }],
      columns: ['kind', 'title']
    });
    const restored = reopened(everything);
    expect(restored.workspace).toBe('everything');
    expect(restored.query).toBe('invoice');
    expect(restored.filters).toEqual(everything.filters);
    expect(restored.groupingChain).toEqual(['year']);
    expect(restored.sort).toEqual(everything.sort);
    expect(restored.activeRow).toBeNull();
  });

  it('reopens a Files view in Files', () => {
    const files = parseExploreURLState(serializeExploreURLState({
      ...everything, workspace: 'files', groupingChain: []
    }));
    expect(canonicalSavedViewState(files).presentation).toBe('files');
    const opened = exploreStateFromSavedView(canonicalSavedViewState(files));
    expect(opened.workspace).toBe('files');
    expect(reopened(files).workspace).toBe('files');
  });

  it.each(['files', 'table'] as const)(
    'opens a %s view with the default filename, type, and file sort',
    (presentation) => {
      expect(exploreStateFromSavedView({ presentation })).toMatchObject({
        fileFilenameQuery: '', fileMIMEFamilies: [],
        fileSort: { field: 'occurred_at', direction: 'desc' }
      });
    }
  );

  it('opens a filter-only view without a stored query in full text', () => {
    expect(canonicalSavedViewState({ ...everything, query: ' ', searchMode: 'semantic' }))
      .not.toHaveProperty('query');
    expect(exploreStateFromSavedView({ presentation: 'timeline' })).toMatchObject({
      workspace: 'everything', query: '', searchMode: 'full_text', filters: [], groupingChain: [],
      presentation: 'timeline', sort: [{ field: 'occurred_at', direction: 'desc' }]
    });
  });

  it('maps the legacy source_id and participant_id fields', () => {
    const opened = exploreStateFromSavedView({
      filters: [
        { field: 'source_id', operator: 'eq', values: ['1'] },
        { field: 'participant_id', operator: 'in', values: ['42'] }
      ]
    });
    expect(opened.filters).toEqual([
      { dimension: 'source', values: ['1'] },
      { dimension: 'participant', values: ['42'] }
    ]);
  });

  it('summarizes the query, filters, grouping, and layout in words', () => {
    expect(savedViewSummary(canonicalSavedViewState(everything))).toEqual([
      'Full text: “invoice”', 'Source: 7', 'Grouped by Year', 'Table'
    ]);
    expect(savedViewSummary({
      query: 'budget', search_mode: 'hybrid',
      filters: [{ field: 'participant_id', operator: 'in', values: ['42', '43'] }],
      grouping: ['source', 'month'], presentation: 'files'
    })).toEqual(['Hybrid: “budget”', 'Person: 42, 43', 'Grouped by Source, then Month', 'Files']);
    expect(savedViewSummary({})).toEqual(['Table']);
  });
});
